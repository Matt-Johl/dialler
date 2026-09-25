// Package adminui is dialler-admin, the operator's web UI (SPEC §6 item
// 9c; protocol/ADMIN-API.md §9): a second process that speaks only to the
// call server's admin API over HTTPS, holds the admin token and a login of
// its own, and keeps no state the API cannot re-read. It never touches
// SIP, media or the wake gateway.
package adminui

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"dialler/server/internal/admin"
	"dialler/server/internal/b2bua"
	"dialler/server/internal/diag"
	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/events"
	"dialler/server/internal/loglevel"
	"dialler/server/internal/status"
)

// Client is the admin API as dialler-admin sees it. Every method is one
// request; the types are the call server's own, so a shape change is a
// compile error here rather than a surprise in a template.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// APIError is a non-2xx answer with the §4.4 envelope decoded.
type APIError struct {
	Status int
	admin.ErrorBody
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("%s (%d)", e.Code(), e.Status)
}

// Code is the stable error code, or the status text when the body was
// not the envelope.
func (e *APIError) Code() string {
	if e.ErrorBody.Error != "" {
		return e.ErrorBody.Error
	}
	return http.StatusText(e.Status)
}

// ErrUnreachable wraps any failure that is not an answer from the server:
// connection refused, timeout, TLS failure. The UI shows one banner for
// it and disables every write (ADMIN-API.md §9).
var ErrUnreachable = errors.New("the call server is not answering")

// Unreachable reports whether err means the server did not answer.
func Unreachable(err error) bool { return errors.Is(err, ErrUnreachable) }

// ErrUnauthorized is a 401: the token is wrong.
var ErrUnauthorized = errors.New("the call server refused the admin token")

// NewClient builds a client for base (https://host:8081) with the bearer
// token. caFile verifies the server's certificate; insecure accepts any
// (the self-signed dev certificate). timeout bounds every request.
func NewClient(base, token, caFile string, insecure bool, timeout time.Duration) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec // -insecure is the dev flag
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("-server-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-server-ca: %s holds no certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http: &http.Client{Timeout: timeout, Transport: &http.Transport{
			TLSClientConfig: tlsCfg, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
		}},
	}, nil
}

// newTestClient is a client over an in-memory transport (tests: no sockets).
func newTestClient(rt http.RoundTripper, token string) *Client {
	return &Client{base: "https://call-server", token: token, http: &http.Client{Transport: rt, Timeout: 5 * time.Second}}
}

// do performs one request. body, when non-nil, is JSON-encoded. ifMatch,
// when non-empty, is sent as If-Match. out, when non-nil, receives the
// decoded 2xx body.
func (c *Client) do(ctx context.Context, method, path string, body any, ifMatch string, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", strconv.Quote(ifMatch))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		e := &APIError{Status: resp.StatusCode}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(raw, &e.ErrorBody)
		return e
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- server ----------------------------------------------------------------

// Whoami checks the token: nil, ErrUnauthorized, or ErrUnreachable.
func (c *Client) Whoami(ctx context.Context) error {
	var v struct {
		OK bool `json:"ok"`
	}
	return c.do(ctx, "GET", "/v1/admin/whoami", nil, "", &v)
}

func (c *Client) Server(ctx context.Context) (status.ServerView, error) {
	var v status.ServerView
	err := c.do(ctx, "GET", "/v1/admin/server", nil, "", &v)
	return v, err
}

func (c *Client) Status(ctx context.Context) (status.FleetView, error) {
	var v status.FleetView
	err := c.do(ctx, "GET", "/v1/admin/status", nil, "", &v)
	return v, err
}

func (c *Client) Calls(ctx context.Context) ([]b2bua.CallView, error) {
	var v []b2bua.CallView
	err := c.do(ctx, "GET", "/v1/admin/calls", nil, "", &v)
	return v, err
}

// EventsPage is GET /v1/admin/events.
type EventsPage struct {
	Next      uint64         `json:"next"`
	Events    []events.Event `json:"events"`
	Truncated bool           `json:"truncated,omitempty"`
}

func (c *Client) Events(ctx context.Context, since uint64, limit int) (EventsPage, error) {
	var v EventsPage
	q := url.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatUint(since, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/admin/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	err := c.do(ctx, "GET", path, nil, "", &v)
	return v, err
}

func (c *Client) Log(ctx context.Context) (loglevel.View, error) {
	var v loglevel.View
	err := c.do(ctx, "GET", "/v1/admin/log", nil, "", &v)
	return v, err
}

// LogRequest is PUT /v1/admin/log; nil fields are left unchanged.
type LogRequest struct {
	Level      *string `json:"level,omitempty"`
	SIPTrace   *bool   `json:"sip_trace,omitempty"`
	ForSeconds *int    `json:"for_seconds,omitempty"`
}

func (c *Client) SetLog(ctx context.Context, req LogRequest) (loglevel.View, error) {
	var v loglevel.View
	err := c.do(ctx, "PUT", "/v1/admin/log", req, "", &v)
	return v, err
}

// ---- devices ---------------------------------------------------------------

func (c *Client) Devices(ctx context.Context) ([]enroll.Device, error) {
	var v []enroll.Device
	err := c.do(ctx, "GET", "/v1/admin/devices", nil, "", &v)
	return v, err
}

func (c *Client) Device(ctx context.Context, id string) (enroll.Device, error) {
	var v enroll.Device
	err := c.do(ctx, "GET", "/v1/admin/devices/"+url.PathEscape(id), nil, "", &v)
	return v, err
}

// Created is POST /v1/admin/devices' answer: the new device with its
// enrolment code and link. Token is present only when the caller sent one.
type Created struct {
	DeviceID    string    `json:"device_id"`
	User        string    `json:"user"`
	Description string    `json:"description"`
	Code        string    `json:"code"`
	ExpiresAt   time.Time `json:"expires_at"`
	URL         string    `json:"url"`
}

func (c *Client) CreateDevice(ctx context.Context, user, description string) (Created, error) {
	var v Created
	err := c.do(ctx, "POST", "/v1/admin/devices", map[string]string{"user": user, "description": description}, "", &v)
	return v, err
}

func (c *Client) SetDescription(ctx context.Context, id, description string, ifMatch string) (enroll.Device, error) {
	var v enroll.Device
	err := c.do(ctx, "PATCH", "/v1/admin/devices/"+url.PathEscape(id), map[string]string{"description": description}, ifMatch, &v)
	return v, err
}

func (c *Client) Revoke(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/admin/devices/"+url.PathEscape(id), nil, "", nil)
}

func (c *Client) Purge(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/admin/devices/"+url.PathEscape(id)+"?purge=1", nil, "", nil)
}

// Code is POST …/enrol-code's answer.
type Code struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	URL       string    `json:"url"`
}

func (c *Client) MintCode(ctx context.Context, id string) (Code, error) {
	var v Code
	err := c.do(ctx, "POST", "/v1/admin/devices/"+url.PathEscape(id)+"/enrol-code", nil, "", &v)
	return v, err
}

func (c *Client) CancelCode(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/admin/devices/"+url.PathEscape(id)+"/enrol-code", nil, "", nil)
}

func (c *Client) SetConfig(ctx context.Context, id string, ssids []string, ifMatch string) (enroll.DeviceConfig, error) {
	var v enroll.DeviceConfig
	err := c.do(ctx, "PUT", "/v1/admin/devices/"+url.PathEscape(id)+"/config", map[string][]string{"ssids": ssids}, ifMatch, &v)
	return v, err
}

func (c *Client) SetLine(ctx context.Context, id, dn, digestUser, secret, ifMatch string) (enroll.PBXLine, error) {
	var v enroll.PBXLine
	body := map[string]string{"digest_user": digestUser, "secret": secret}
	if dn != "" {
		body["dn"] = dn
	}
	err := c.do(ctx, "PUT", "/v1/admin/devices/"+url.PathEscape(id)+"/pbx-line", body, ifMatch, &v)
	return v, err
}

func (c *Client) DeleteLine(ctx context.Context, id, ifMatch string) error {
	return c.do(ctx, "DELETE", "/v1/admin/devices/"+url.PathEscape(id)+"/pbx-line", nil, ifMatch, nil)
}

// ---- directory -------------------------------------------------------------

func (c *Client) Directory(ctx context.Context, id string) (directory.ListResponse, error) {
	var v directory.ListResponse
	err := c.do(ctx, "GET", "/v1/admin/devices/"+url.PathEscape(id)+"/directory", nil, "", &v)
	return v, err
}

// ReplaceDirectory is the replace-all; dryRun previews the counts.
func (c *Client) ReplaceDirectory(ctx context.Context, id string, contacts []directory.Contact, ifMatch string, dryRun bool) (directory.ReplaceResult, error) {
	var v directory.ReplaceResult
	path := "/v1/admin/devices/" + url.PathEscape(id) + "/directory"
	if dryRun {
		path += "?dry_run=1"
		ifMatch = ""
	}
	if contacts == nil {
		contacts = []directory.Contact{}
	}
	err := c.do(ctx, "PUT", path, map[string]any{"contacts": contacts}, ifMatch, &v)
	return v, err
}

func (c *Client) AddContact(ctx context.Context, id string, ct directory.Contact, ifMatch string) (directory.Contact, error) {
	var v directory.Contact
	err := c.do(ctx, "POST", "/v1/admin/devices/"+url.PathEscape(id)+"/directory", ct, ifMatch, &v)
	return v, err
}

func (c *Client) UpdateContact(ctx context.Context, id string, ct directory.Contact, ifMatch string) (directory.Contact, error) {
	var v directory.Contact
	err := c.do(ctx, "PUT", "/v1/admin/devices/"+url.PathEscape(id)+"/directory/"+url.PathEscape(ct.ID), ct, ifMatch, &v)
	return v, err
}

func (c *Client) DeleteContact(ctx context.Context, id, cid, ifMatch string) error {
	return c.do(ctx, "DELETE", "/v1/admin/devices/"+url.PathEscape(id)+"/directory/"+url.PathEscape(cid), nil, ifMatch, nil)
}

// ---- diagnostics -----------------------------------------------------------

func (c *Client) Diag(ctx context.Context, id string) ([]diag.Entry, error) {
	var v []diag.Entry
	err := c.do(ctx, "GET", "/v1/admin/devices/"+url.PathEscape(id)+"/diag", nil, "", &v)
	return v, err
}

// DiagFile streams one upload; the caller closes the body.
func (c *Client) DiagFile(ctx context.Context, id, name string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/admin/devices/"+url.PathEscape(id)+"/diag/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		e := &APIError{Status: resp.StatusCode}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(raw, &e.ErrorBody)
		return nil, e
	}
	return resp, nil
}

func (c *Client) DeleteDiag(ctx context.Context, id, name string) error {
	path := "/v1/admin/devices/" + url.PathEscape(id) + "/diag"
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return c.do(ctx, "DELETE", path, nil, "", nil)
}
