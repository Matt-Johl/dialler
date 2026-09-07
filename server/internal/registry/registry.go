// Package registry is the light server's location table: which SIP users
// exist, which device can be woken for each, and whether the user currently
// holds a live SIP registration (contact + expiry).
package registry

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Endpoint is one provisioned user.
type Endpoint struct {
	User     string    // SIP user part, e.g. "201"
	DeviceID string    // wake-routable identity; "" if none
	Contact  string    // SIP contact URI while registered
	Expires  time.Time // registration expiry; zero when unregistered
}

// ErrUnknownUser is returned when registering an unprovisioned user.
var ErrUnknownUser = errors.New("registry: unknown user")

// Registry is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	now      func() time.Time
	byUser   map[string]*Endpoint
	byDevice map[string]string          // deviceID → user
	waiters  map[string][]chan struct{} // user → callers blocked in WaitRegistered
}

// New creates an empty registry. now may be nil.
func New(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		now:      now,
		byUser:   map[string]*Endpoint{},
		byDevice: map[string]string{},
		waiters:  map[string][]chan struct{}{},
	}
}

// WaitRegistered blocks until user holds a live SIP registration or ctx ends.
// It returns immediately if the user is already registered. This is the hook
// the call controller uses after sending a wake: the woken app registers,
// and the pending INVITE proceeds.
func (r *Registry) WaitRegistered(ctx context.Context, user string) (Endpoint, error) {
	r.mu.Lock()
	if ep := r.byUser[user]; ep != nil {
		if v := r.viewLocked(ep); v.Contact != "" {
			r.mu.Unlock()
			return v, nil
		}
	}
	ch := make(chan struct{})
	r.waiters[user] = append(r.waiters[user], ch)
	r.mu.Unlock()

	select {
	case <-ch:
		ep, ok := r.Lookup(user)
		if !ok || ep.Contact == "" {
			return Endpoint{}, ErrUnknownUser
		}
		return ep, nil
	case <-ctx.Done():
		r.mu.Lock()
		ws := r.waiters[user]
		for i, w := range ws {
			if w == ch {
				r.waiters[user] = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		if len(r.waiters[user]) == 0 {
			delete(r.waiters, user)
		}
		r.mu.Unlock()
		return Endpoint{}, ctx.Err()
	}
}

// notifyLocked wakes every WaitRegistered caller for user. Caller holds mu.
func (r *Registry) notifyLocked(user string) {
	for _, ch := range r.waiters[user] {
		close(ch)
	}
	delete(r.waiters, user)
}

// Provision declares that user exists and is woken via deviceID. Calling it
// again for the same user rebinds the device and keeps any registration.
func (r *Registry) Provision(user, deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ep := r.byUser[user]
	if ep == nil {
		ep = &Endpoint{User: user}
		r.byUser[user] = ep
	}
	if ep.DeviceID != "" {
		delete(r.byDevice, ep.DeviceID)
	}
	ep.DeviceID = deviceID
	if deviceID != "" {
		r.byDevice[deviceID] = user
	}
}

// Deprovision removes the user entirely.
func (r *Registry) Deprovision(user string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ep := r.byUser[user]; ep != nil && ep.DeviceID != "" {
		delete(r.byDevice, ep.DeviceID)
	}
	delete(r.byUser, user)
}

// Register records a live SIP contact for user with the given TTL.
func (r *Registry) Register(user, contact string, ttl time.Duration) (Endpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ep := r.byUser[user]
	if ep == nil {
		return Endpoint{}, ErrUnknownUser
	}
	ep.Contact = contact
	ep.Expires = r.now().Add(ttl)
	r.notifyLocked(user)
	return *ep, nil
}

// Unregister clears the SIP contact for user.
func (r *Registry) Unregister(user string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ep := r.byUser[user]; ep != nil {
		ep.Contact, ep.Expires = "", time.Time{}
	}
}

// Lookup returns the endpoint for user. An expired registration is reported
// with Contact cleared so callers never route to a stale contact.
func (r *Registry) Lookup(user string) (Endpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ep := r.byUser[user]
	if ep == nil {
		return Endpoint{}, false
	}
	return r.viewLocked(ep), true
}

// LookupDevice finds the endpoint woken via deviceID.
func (r *Registry) LookupDevice(deviceID string) (Endpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	user, ok := r.byDevice[deviceID]
	if !ok {
		return Endpoint{}, false
	}
	return r.viewLocked(r.byUser[user]), true
}

// Registered reports whether user has an unexpired SIP registration.
func (r *Registry) Registered(user string) bool {
	ep, ok := r.Lookup(user)
	return ok && ep.Contact != ""
}

// Users lists provisioned users, sorted.
func (r *Registry) Users() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byUser))
	for u := range r.byUser {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) viewLocked(ep *Endpoint) Endpoint {
	v := *ep
	if v.Contact != "" && !v.Expires.After(r.now()) {
		v.Contact, v.Expires = "", time.Time{}
	}
	return v
}
