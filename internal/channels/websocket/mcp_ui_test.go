package websocket

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeMCP is an in-memory MCPStore.
type fakeMCP struct {
	servers map[string]MCPServerView
	lastPut MCPServerInput
}

func (f *fakeMCP) MCPServers() (MCPList, error) {
	out := MCPList{Servers: []MCPServerView{}, RestartRequired: []string{}}
	for _, v := range f.servers {
		out.Servers = append(out.Servers, v)
	}
	return out, nil
}

func (f *fakeMCP) PutMCPServer(name string, in MCPServerInput) (MCPServerView, bool, error) {
	if in.Command == "" && in.URL == "" {
		return MCPServerView{}, false, fmt.Errorf("%w: command or url is required", ErrInvalidMCP)
	}
	f.lastPut = in
	_, exists := f.servers[name]
	v := MCPServerView{Name: name, Enabled: true, Command: in.Command, URL: in.URL}
	if in.Env != nil {
		for k := range *in.Env {
			v.EnvKeys = append(v.EnvKeys, k)
		}
	}
	f.servers[name] = v
	return v, !exists, nil
}

func (f *fakeMCP) SetMCPServerEnabled(name string, on bool) (MCPServerView, error) {
	v, ok := f.servers[name]
	if !ok {
		return MCPServerView{}, ErrMCPNotFound
	}
	v.Enabled = on
	f.servers[name] = v
	return v, nil
}

func (f *fakeMCP) DeleteMCPServer(name string) error {
	if _, ok := f.servers[name]; !ok {
		return ErrMCPNotFound
	}
	delete(f.servers, name)
	return nil
}

func TestMCPRoutes(t *testing.T) {
	// Unmounted without a store.
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	if code, _ := skillsRequest(t, h, http.MethodGet, MCPPath, ""); code != http.StatusNotFound {
		t.Errorf("GET without store = %d", code)
	}

	store := &fakeMCP{servers: map[string]MCPServerView{}}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", MCP: store})
	code, body := skillsRequest(t, h, http.MethodGet, MCPPath, "")
	if code != http.StatusOK || string(body) != `{"servers":[],"live":false,"restart_required":[]}`+"\n" {
		t.Errorf("GET = %d %s", code, body)
	}

	code, body = skillsRequest(t, h, http.MethodPut, MCPPath+"/fs", `{"command":"npx","args":["-y"],"env":{"TOKEN":"sk-secret"}}`)
	if code != http.StatusCreated || strings.Contains(string(body), "sk-secret") {
		t.Errorf("create = %d %s", code, body)
	}
	if store.lastPut.Env == nil || (*store.lastPut.Env)["TOKEN"] != "sk-secret" {
		t.Errorf("put input = %+v", store.lastPut)
	}
	if code, body = skillsRequest(t, h, http.MethodPut, MCPPath+"/fs", `{"command":"uvx"}`); code != http.StatusOK || store.lastPut.Env != nil {
		t.Errorf("replace = %d %s, env %v", code, body, store.lastPut.Env)
	}
	for _, bad := range []string{`{"command":"x","bogus":1}`, `[]`, ``} {
		if code, _ := skillsRequest(t, h, http.MethodPut, MCPPath+"/fs", bad); code != http.StatusBadRequest {
			t.Errorf("put %q = %d", bad, code)
		}
	}
	if code, body = skillsRequest(t, h, http.MethodPut, MCPPath+"/x", `{}`); code != http.StatusBadRequest || !strings.Contains(string(body), "command or url is required") {
		t.Errorf("invalid = %d %s", code, body)
	}

	code, body = skillsRequest(t, h, http.MethodPatch, MCPPath+"/fs", `{"enabled":false}`)
	var v MCPServerView
	if json.Unmarshal(body, &v); code != http.StatusOK || v.Enabled {
		t.Errorf("patch = %d %s", code, body)
	}
	if code, _ := skillsRequest(t, h, http.MethodPatch, MCPPath+"/fs", `{}`); code != http.StatusBadRequest {
		t.Errorf("empty patch = %d", code)
	}
	if code, _ := skillsRequest(t, h, http.MethodPatch, MCPPath+"/nope", `{"enabled":true}`); code != http.StatusNotFound {
		t.Errorf("patch missing = %d", code)
	}

	// Cross-origin mutations are refused.
	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+MCPPath+"/fs", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || store.servers["fs"].Name == "" {
		t.Errorf("cross-origin delete = %d", resp.StatusCode)
	}
	// Non-local requests without sign-in are refused.
	req, _ = http.NewRequest(http.MethodGet, h.srv.URL+MCPPath, nil)
	req.Host = "evil.example"
	if resp, err = h.srv.Client().Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host GET = %d", resp.StatusCode)
	}

	if code, _ := skillsRequest(t, h, http.MethodDelete, MCPPath+"/fs", ""); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if code, _ := skillsRequest(t, h, http.MethodDelete, MCPPath+"/fs", ""); code != http.StatusNotFound {
		t.Errorf("delete again = %d", code)
	}
}
