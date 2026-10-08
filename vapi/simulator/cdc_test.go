// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/library"
	vapi "github.com/vmware/govmomi/vapi/simulator"
	"github.com/vmware/govmomi/vim25"
)

// clsClient makes vAPI JSON-RPC 2.0 calls to the /cls endpoint, in the shape the vAPI Go runtime writes.
type clsClient struct {
	t   *testing.T
	vc  *vim25.Client
	url string
	sec map[string]any
}

func (c *clsClient) call(ctx context.Context, service, operation string, input map[string]any) map[string]any {
	c.t.Helper()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "invoke",
		"params": map[string]any{
			"serviceId":   service,
			"operationId": operation,
			"input":       map[string]any{"STRUCTURE": map[string]any{"operation-input": input}},
			"ctx":         map[string]any{"securityCtx": c.sec, "appCtx": map[string]any{}},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	hreq.Header.Set("Content-Type", "application/json")

	res, err := c.vc.Client.Client.Do(hreq)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("%s.%s: status %s", service, operation, res.Status)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		c.t.Fatalf("%s.%s: Content-Type=%q", service, operation, ct)
	}

	var rpc struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Result  map[string]any `json:"result"`
	}
	if err := json.NewDecoder(res.Body).Decode(&rpc); err != nil {
		c.t.Fatal(err)
	}
	if rpc.JSONRPC != "2.0" || rpc.ID != "1" || rpc.Result == nil {
		c.t.Fatalf("%s.%s: invalid response %+v", service, operation, rpc)
	}
	return rpc.Result
}

// output returns the result's output, failing if the result is an error.
func (c *clsClient) output(ctx context.Context, service, operation string, input map[string]any) any {
	c.t.Helper()

	res := c.call(ctx, service, operation, input)
	out, ok := res["output"]
	if !ok {
		c.t.Fatalf("%s.%s: %v", service, operation, res)
	}
	return out
}

// fails checks that the call's result is the com.vmware.vapi.std.errors error of the given name.
func (c *clsClient) fails(ctx context.Context, service, operation string, input map[string]any, name string) {
	c.t.Helper()

	res := c.call(ctx, service, operation, input)
	errs, _ := res["error"].(map[string]any)
	e, _ := errs["ERROR"].(map[string]any)
	if _, ok := e["com.vmware.vapi.std.errors."+name]; !ok {
		c.t.Fatalf("%s.%s: expected %s, got %v", service, operation, name, res)
	}
}

// structure returns the fields of the named vAPI structure value v.
func structure(t *testing.T, v any, name string) map[string]any {
	t.Helper()

	s, _ := v.(map[string]any)["STRUCTURE"].(map[string]any)
	fields, ok := s[name].(map[string]any)
	if !ok {
		t.Fatalf("expected structure %s, got %v", name, v)
	}
	return fields
}

// optional returns the value of vAPI optional value v.
func optional(t *testing.T, v any) any {
	t.Helper()

	o, ok := v.(map[string]any)
	if _, set := o["OPTIONAL"]; !ok || !set {
		t.Fatalf("expected optional, got %v", v)
	}
	return o["OPTIONAL"]
}

type change struct {
	Type, ID, Kind       string
	ParentType, ParentID string
}

func changes(t *testing.T, list any) (string, []change) {
	t.Helper()

	fields := structure(t, list, "com.vmware.cdc.change_log.change_list")
	seq, _ := fields["sequence"].(string)

	var res []change
	for _, e := range optional(t, fields["changes"]).([]any) {
		ev := structure(t, e, "com.vmware.cdc.change_log.change_event")
		id := structure(t, ev["resource"], "com.vmware.vapi.std.dynamic_ID")
		c := change{Type: id["type"].(string), ID: id["id"].(string), Kind: ev["kind"].(string)}
		if parents, ok := optional(t, ev["parent_resources"]).([]any); ok {
			p := structure(t, parents[0], "com.vmware.vapi.std.dynamic_ID")
			c.ParentType, c.ParentID = p["type"].(string), p["id"].(string)
		}
		optional(t, ev["properties"])
		res = append(res, c)
	}
	return seq, res
}

const (
	sessionService   = "com.vmware.cis.session"
	changeLogService = "com.vmware.cdc.change_log"
	libraryModel     = "com.vmware.content.LibraryModel"
	itemModel        = "com.vmware.content.library.ItemModel"
)

func TestChangeLog(t *testing.T) {
	wait := vapi.ChangeLogWait
	vapi.ChangeLogWait = 100 * time.Millisecond
	defer func() { vapi.ChangeLogWait = wait }()

	simulator.Test(func(ctx context.Context, vc *vim25.Client) {
		u := *vc.URL()
		u.Path = vapi.CLSPath
		c := &clsClient{t: t, vc: vc, url: u.String()}

		c.fails(ctx, sessionService, "create", nil, "unauthenticated")
		c.fails(ctx, changeLogService, "get_current_sequence", nil, "unauthenticated")

		// IRO creates its session with a SAML bearer token, then uses the session id.
		c.sec = map[string]any{
			"schemeId":  "com.vmware.vapi.std.security.saml_bearer_token",
			"samlToken": `<saml2:Assertion xmlns:saml2="urn:oasis:names:tc:SAML:2.0:assertion"><saml2:Subject><saml2:NameID>iro@vsphere.local</saml2:NameID></saml2:Subject></saml2:Assertion>`,
		}
		secret, _ := c.output(ctx, sessionService, "create", nil).(map[string]any)
		id, _ := secret["SECRET"].(string)
		if id == "" {
			t.Fatalf("session create: %v", secret)
		}
		c.sec = map[string]any{"schemeId": "com.vmware.vapi.std.security.session_id", "sessionId": id}

		info := structure(t, c.output(ctx, sessionService, "get", nil), "com.vmware.cis.session.info")
		if info["user"] != "iro@vsphere.local" {
			t.Errorf("session user=%v", info["user"])
		}
		for _, field := range []string{"created_time", "last_accessed_time"} {
			if _, err := time.Parse(time.RFC3339Nano, info[field].(string)); err != nil {
				t.Errorf("session %s: %s", field, err)
			}
		}

		seq, ok := c.output(ctx, changeLogService, "get_current_sequence", nil).(string)
		if !ok || seq == "" {
			t.Fatalf("get_current_sequence: %v", seq)
		}

		m, libID := newLibrary(ctx, t, vc)
		itemID, err := m.CreateLibraryItem(ctx, library.Item{Name: "item", Type: library.ItemTypeOVF, LibraryID: libID})
		if err != nil {
			t.Fatal(err)
		}

		next, events := changes(t, c.output(ctx, changeLogService, "get_changes", map[string]any{"sequence_token": seq}))
		expect := []change{
			{Type: libraryModel, ID: libID, Kind: "CREATE"},
			{Type: itemModel, ID: itemID, Kind: "CREATE", ParentType: libraryModel, ParentID: libID},
		}
		if len(events) != len(expect) || events[0] != expect[0] || events[1] != expect[1] {
			t.Fatalf("get_changes=%+v, expected %+v", events, expect)
		}
		if next == seq {
			t.Fatalf("get_changes sequence unchanged: %q", next)
		}

		// With no changes pending, get_changes waits, then returns none and the same sequence.
		start := time.Now()
		same, events := changes(t, c.output(ctx, changeLogService, "get_changes", map[string]any{"sequence_token": next}))
		if len(events) != 0 || same != next {
			t.Fatalf("get_changes=%q %+v, expected %q and no changes", same, events, next)
		}
		if elapsed := time.Since(start); elapsed < vapi.ChangeLogWait {
			t.Errorf("get_changes returned after %s, expected a wait of %s", elapsed, vapi.ChangeLogWait)
		}

		item, err := m.GetLibraryItem(ctx, itemID)
		if err != nil {
			t.Fatal(err)
		}
		if err = m.DeleteLibraryItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		_, events = changes(t, c.output(ctx, changeLogService, "get_changes", map[string]any{"sequence_token": next}))
		if len(events) != 1 || events[0].ID != itemID || events[0].Kind != "DELETE" {
			t.Fatalf("get_changes=%+v, expected the item's DELETE", events)
		}

		for _, token := range []string{"x", "-1", "1000"} {
			c.fails(ctx, changeLogService, "get_changes", map[string]any{"sequence_token": token}, "invalid_argument")
		}

		c.output(ctx, sessionService, "delete", nil)
		c.fails(ctx, changeLogService, "get_current_sequence", nil, "unauthenticated")
	})
}
