// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/internal"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/rest"
	vim "github.com/vmware/govmomi/vim25/types"
)

// CLSPath is the Content Library Service vAPI JSON-RPC endpoint, which serves
// com.vmware.cis.session and the com.vmware.cdc.change_log change data capture service.
const CLSPath = "/cls"

// ChangeLogWait is how long com.vmware.cdc.change_log get_changes waits for a change when
// none are pending. vCenter waits up to 5 minutes; vcsim waits briefly, as an in-flight
// request delays server shutdown.
var ChangeLogWait = 5 * time.Second

// Resource types of the content library change events, as consumed by image-registry-operator.
const (
	libraryResourceType = "com.vmware.content.LibraryModel"
	itemResourceType    = "com.vmware.content.library.ItemModel"
)

// Change kinds of com.vmware.cdc.change_log.change_event.change_kind.
const (
	changeCreate = "CREATE"
	changeUpdate = "UPDATE"
	changeDelete = "DELETE"
)

// vAPI authentication scheme ids, security context keys and the datetime layout.
const (
	schemeID         = "schemeId"
	samlBearerScheme = "com.vmware.vapi.std.security.saml_bearer_token"
	userPassScheme   = "com.vmware.vapi.std.security.user_pass"
	sessionScheme    = "com.vmware.vapi.std.security.session_id"
	samlTokenKey     = "samlToken"
	userNameKey      = "userName"
	passwordKey      = "password"
	sessionIDKey     = "sessionId"
	vapiDateTime     = "2006-01-02T15:04:05.000Z"
)

type dynamicID struct {
	Type string
	ID   string
}

type changeEvent struct {
	Resource   dynamicID
	Kind       string
	Properties []string
	Parent     *dynamicID
}

// changeLog is the ordered log of content library changes served by get_changes.
// A sequence token is the decimal index of the next event to read.
// It has its own lock, as get_changes waits without holding the handler's.
type changeLog struct {
	mu     sync.Mutex
	events []changeEvent
	notify chan struct{}
}

func (c *changeLog) add(e changeEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.events = append(c.events, e)
	if c.notify != nil {
		close(c.notify)
		c.notify = nil
	}
}

func (c *changeLog) current() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return strconv.Itoa(len(c.events))
}

// since returns the events after token and the token that follows them, waiting up to
// wait for an event when none are pending. ok is false if the token is not one issued.
func (c *changeLog) since(ctx context.Context, token string, wait time.Duration) ([]changeEvent, string, bool) {
	start, err := strconv.Atoi(token)
	if err != nil || start < 0 {
		return nil, "", false
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		c.mu.Lock()
		n := len(c.events)
		if start > n {
			c.mu.Unlock()
			return nil, "", false
		}
		if start < n {
			events := append([]changeEvent(nil), c.events[start:]...)
			c.mu.Unlock()
			return events, strconv.Itoa(n), true
		}
		if c.notify == nil {
			c.notify = make(chan struct{})
		}
		notify := c.notify
		c.mu.Unlock()

		select {
		case <-notify:
		case <-timer.C:
			return nil, token, true
		case <-ctx.Done():
			return nil, token, true
		}
	}
}

// libraryChanged records a change to library l.
func (s *handler) libraryChanged(kind string, l *library.Library, props ...string) {
	s.Changes.add(changeEvent{
		Resource:   dynamicID{Type: libraryResourceType, ID: l.ID},
		Kind:       kind,
		Properties: props,
	})
}

// itemChanged records a change to item i, whose parent is its library.
func (s *handler) itemChanged(kind string, i *library.Item, props ...string) {
	s.Changes.add(changeEvent{
		Resource:   dynamicID{Type: itemResourceType, ID: i.ID},
		Kind:       kind,
		Properties: props,
		Parent:     &dynamicID{Type: libraryResourceType, ID: i.LibraryID},
	})
}

// libraryProperties returns the names of the library properties set in update spec u.
func libraryProperties(u *library.Library) []string {
	var props []string
	if u.Name != "" {
		props = append(props, "name")
	}
	if u.Description != nil {
		props = append(props, "description")
	}
	if u.Configuration != nil {
		props = append(props, "configuration")
	}
	return append(props, "version")
}

// itemProperties returns the names of the item properties set in update spec u.
func itemProperties(u *library.Item) []string {
	var props []string
	if u.Name != "" {
		props = append(props, "name")
	}
	if u.Description != nil {
		props = append(props, "description")
	}
	if u.Type != "" {
		props = append(props, "type")
	}
	return props
}

// The vAPI JSON-RPC data value encoding: a structure is {"STRUCTURE": {name: fields}}, an
// optional is {"OPTIONAL": value-or-null}, an error is {"ERROR": {name: fields}}, a secret is
// {"SECRET": value}, and strings and lists are plain JSON.

func vapiStruct(name string, fields map[string]any) map[string]any {
	return map[string]any{"STRUCTURE": map[string]any{name: fields}}
}

func vapiOptional(val any) map[string]any {
	return map[string]any{"OPTIONAL": val}
}

func vapiDynamicID(id dynamicID) map[string]any {
	return vapiStruct("com.vmware.vapi.std.dynamic_ID", map[string]any{
		"type": id.Type,
		"id":   id.ID,
	})
}

func (e changeEvent) value() map[string]any {
	var props, parents any
	if e.Properties != nil {
		props = e.Properties
	}
	if e.Parent != nil {
		parents = []any{vapiDynamicID(*e.Parent)}
	}
	return vapiStruct("com.vmware.cdc.change_log.change_event", map[string]any{
		"resource":         vapiDynamicID(e.Resource),
		"kind":             e.Kind,
		"properties":       vapiOptional(props),
		"parent_resources": vapiOptional(parents),
	})
}

// methodOutput is a successful vAPI method result.
func methodOutput(val any) map[string]any {
	return map[string]any{"output": val}
}

// methodError is a vAPI method result for the com.vmware.vapi.std.errors error of the given type,
// for example "invalid_argument" and "INVALID_ARGUMENT".
func methodError(name, kind string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"ERROR": map[string]any{
				"com.vmware.vapi.std.errors." + name: map[string]any{
					"messages":   []any{},
					"data":       vapiOptional(nil),
					"error_type": vapiOptional(kind),
				},
			},
		},
	}
}

var errUnauthenticated = methodError("unauthenticated", "UNAUTHENTICATED")

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Params  struct {
		ServiceID   string          `json:"serviceId"`
		OperationID string          `json:"operationId"`
		Input       json.RawMessage `json:"input"`
		Ctx         struct {
			SecurityCtx map[string]any `json:"securityCtx"`
		} `json:"ctx"`
	} `json:"params"`
}

// cls serves the Content Library Service vAPI JSON-RPC 2.0 endpoint. It is not wrapped by
// HandleFunc, as get_changes must wait without holding the handler's lock.
func (s *handler) cls(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req jsonRPCRequest
	if !Decode(r, w, &req) {
		return
	}

	StatusOK(w, map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result":  s.clsInvoke(r, &req),
	})
}

func (s *handler) clsInvoke(r *http.Request, req *jsonRPCRequest) map[string]any {
	p := &req.Params
	sec := p.Ctx.SecurityCtx

	switch p.ServiceID + "." + p.OperationID {
	case "com.vmware.cis.session.create":
		return s.clsSessionCreate(r, sec)
	case "com.vmware.cis.session.get":
		s.Lock()
		defer s.Unlock()
		session := s.clsSession(r, sec)
		if session == nil {
			return errUnauthenticated
		}
		return methodOutput(vapiStruct("com.vmware.cis.session.info", map[string]any{
			"user":               session.User,
			"created_time":       session.Created.UTC().Format(vapiDateTime),
			"last_accessed_time": session.LastAccessed.UTC().Format(vapiDateTime),
		}))
	case "com.vmware.cis.session.delete":
		s.Lock()
		defer s.Unlock()
		if s.clsSession(r, sec) == nil {
			return errUnauthenticated
		}
		delete(s.Session, clsSessionID(r, sec))
		return methodOutput(nil)
	case "com.vmware.cdc.change_log.get_current_sequence":
		if !s.clsAuthenticated(r, sec) {
			return errUnauthenticated
		}
		return methodOutput(s.Changes.current())
	case "com.vmware.cdc.change_log.get_changes":
		if !s.clsAuthenticated(r, sec) {
			return errUnauthenticated
		}
		var input struct {
			Structure struct {
				Input struct {
					SequenceToken *string `json:"sequence_token"`
				} `json:"operation-input"`
			} `json:"STRUCTURE"`
		}
		if json.Unmarshal(p.Input, &input) != nil || input.Structure.Input.SequenceToken == nil {
			return methodError("invalid_argument", "INVALID_ARGUMENT")
		}
		events, next, ok := s.Changes.since(r.Context(), *input.Structure.Input.SequenceToken, ChangeLogWait)
		if !ok {
			return methodError("invalid_argument", "INVALID_ARGUMENT")
		}
		changes := []any{}
		for _, e := range events {
			changes = append(changes, e.value())
		}
		return methodOutput(vapiStruct("com.vmware.cdc.change_log.change_list", map[string]any{
			"sequence": next,
			"changes":  vapiOptional(changes),
		}))
	default:
		return methodError("operation_not_found", "OPERATION_NOT_FOUND")
	}
}

// clsSessionCreate creates a session for a SAML bearer token or a user name and password.
func (s *handler) clsSessionCreate(r *http.Request, sec map[string]any) map[string]any {
	str := func(key string) string {
		v, _ := sec[key].(string)
		return v
	}

	var user string
	switch str(schemeID) {
	case samlBearerScheme:
		token := str(samlTokenKey)
		if token == "" {
			return errUnauthenticated
		}
		user = nameID(token)
		if valid := s.Map.SessionManager().ValidToken; valid != nil {
			id, err := valid(token)
			if err != nil {
				return errUnauthenticated
			}
			user = id.Subject
		}
	case userPassScheme:
		user = str(userNameKey)
		login := &vim.Login{UserName: user, Password: str(passwordKey)}
		if !s.Map.SessionManager().Authenticate(s.URL, login) {
			return errUnauthenticated
		}
	default:
		return errUnauthenticated
	}

	s.Lock()
	defer s.Unlock()

	id := uuid.New().String()
	now := time.Now()
	s.Session[id] = &rest.Session{User: user, Created: now, LastAccessed: now}
	simulator.SessionIdleWatch(s.newContext(), id, s.expiredSession)

	return methodOutput(map[string]any{"SECRET": id})
}

// clsSessionID returns the session id of a request authenticated with the session_id scheme,
// either in its security context or its vmware-api-session-id header.
func clsSessionID(r *http.Request, sec map[string]any) string {
	if sec[schemeID] == sessionScheme {
		if id, ok := sec[sessionIDKey].(string); ok {
			return id
		}
	}
	return r.Header.Get(internal.SessionCookieName)
}

// clsSession returns the request's session, or nil. The caller holds the handler's lock.
func (s *handler) clsSession(r *http.Request, sec map[string]any) *rest.Session {
	session, ok := s.Session[clsSessionID(r, sec)]
	if !ok {
		return nil
	}
	session.LastAccessed = time.Now()
	return session
}

func (s *handler) clsAuthenticated(r *http.Request, sec map[string]any) bool {
	s.Lock()
	defer s.Unlock()

	return s.clsSession(r, sec) != nil
}
