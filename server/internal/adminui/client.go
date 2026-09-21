// Package adminui is dialler-admin: the operator's web interface to the
// call server (SPEC §4.8, §6 item 9). It holds the admin token and a login
// of its own, speaks only to the call server's admin API over HTTPS, and
// renders server-side HTML with no scripting. It can be restarted or
// redeployed with no effect on a call: the call server never knows it is
// there.
package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/status"
)

// Client is the typed view of the call server's admin API.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient talks to the admin API at base (https://host:8080) with the
// admin bearer token, over transport (which decides how the call server's
// certificate is trusted).
func NewClient(base *url.URL, token string, transport http.RoundTripper) *Client {
	return &Client{base: base, token: token, http: &http.Client{Transport: transport, Timeout: 15 * time.Second}}
}

// APIError is a non-2xx answer from the call server.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("call server answered %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// IsNotFound reports whether err is the call server's 404.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	// path may carry a query ("…?purge=1"); parsed, not treated as a path,
	// or the server would see an escaped "?" and do the wrong thing.
	ref, err := url.Parse(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.ResolveReference(ref).String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call server unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Status is the server's identity and every device's live state.
func (c *Client) Status(ctx context.Context) (status.Response, error) {
	var out status.Response
	err := c.do(ctx, http.MethodGet, "/v1/admin/status", nil, &out)
	return out, err
}

// Created is what adding a device returns: its identity and the enrolment
// code to show once.
type Created struct {
	DeviceID  string    `json:"device_id"`
	User      string    `json:"user"`
	Label     string    `json:"label"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	URL       string    `json:"url"`
}

// CreateDevice adds a device for user with label; the server generates
// the id and mints the first enrolment code.
func (c *Client) CreateDevice(ctx context.Context, user, label string) (Created, error) {
	var out Created
	err := c.do(ctx, http.MethodPost, "/v1/admin/devices", map[string]string{"user": user, "label": label}, &out)
	return out, err
}

// UpdateDevice changes a device's extension and label; its credential,
// code and settings stay.
func (c *Client) UpdateDevice(ctx context.Context, deviceID, user, label string) (enroll.Device, error) {
	var out enroll.Device
	err := c.do(ctx, http.MethodPut, "/v1/admin/devices/"+url.PathEscape(deviceID), map[string]string{"user": user, "label": label}, &out)
	return out, err
}

// Code is a freshly minted enrolment code.
type Code struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	URL       string    `json:"url"`
}

// NewCode mints a fresh enrolment code for an existing device.
func (c *Client) NewCode(ctx context.Context, deviceID string) (Code, error) {
	var out Code
	err := c.do(ctx, http.MethodPost, "/v1/admin/devices/"+url.PathEscape(deviceID)+"/enrol-code", nil, &out)
	return out, err
}

// Revoke disables a device's credential; the record and directory stay.
func (c *Client) Revoke(ctx context.Context, deviceID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/admin/devices/"+url.PathEscape(deviceID), nil, nil)
}

// Purge deletes a device for good: record, directory and settings.
func (c *Client) Purge(ctx context.Context, deviceID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/admin/devices/"+url.PathEscape(deviceID)+"?purge=1", nil, nil)
}

// Directory is a device's live contacts and version.
func (c *Client) Directory(ctx context.Context, deviceID string) (directory.ListResponse, error) {
	var out directory.ListResponse
	err := c.do(ctx, http.MethodGet, "/v1/admin/devices/"+url.PathEscape(deviceID)+"/directory", nil, &out)
	return out, err
}

// ReplaceDirectory makes contacts the whole of a device's directory.
func (c *Client) ReplaceDirectory(ctx context.Context, deviceID string, contacts []directory.Contact) (directory.ReplaceResult, error) {
	var out directory.ReplaceResult
	err := c.do(ctx, http.MethodPut, "/v1/admin/devices/"+url.PathEscape(deviceID)+"/directory", directory.ListResponse{Contacts: contacts}, &out)
	return out, err
}

// UpsertContact adds a contact (empty ID) or changes one.
func (c *Client) UpsertContact(ctx context.Context, deviceID string, contact directory.Contact) (directory.Contact, error) {
	var out directory.Contact
	path := "/v1/admin/devices/" + url.PathEscape(deviceID) + "/directory"
	method := http.MethodPost
	if contact.ID != "" {
		path += "/" + url.PathEscape(contact.ID)
		method = http.MethodPut
	}
	err := c.do(ctx, method, path, contact, &out)
	return out, err
}

// DeleteContact tombstones one contact.
func (c *Client) DeleteContact(ctx context.Context, deviceID, contactID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/admin/devices/"+url.PathEscape(deviceID)+"/directory/"+url.PathEscape(contactID), nil, nil)
}

// Config is a device's server-managed settings, or nil when none are set.
func (c *Client) Config(ctx context.Context, deviceID string) (*enroll.DeviceConfig, error) {
	var out enroll.DeviceConfig
	err := c.do(ctx, http.MethodGet, "/v1/admin/devices/"+url.PathEscape(deviceID)+"/config", nil, &out)
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetConfig replaces a device's SSID list; the server pushes it.
func (c *Client) SetConfig(ctx context.Context, deviceID string, ssids []string) (enroll.DeviceConfig, error) {
	var out enroll.DeviceConfig
	err := c.do(ctx, http.MethodPut, "/v1/admin/devices/"+url.PathEscape(deviceID)+"/config", map[string][]string{"ssids": ssids}, &out)
	return out, err
}
