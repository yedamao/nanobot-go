package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/cron"
)

func TestCronUIListRunRemove(t *testing.T) {
	svc, err := cron.NewService(cron.ServiceConfig{
		Store: cron.NewStore(t.TempDir()),
		Run:   func(context.Context, cron.Job) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.Add(cron.AddSpec{
		Name:     "stretch",
		Message:  "stretch now",
		Schedule: cron.Schedule{Kind: cron.KindEvery, Every: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterSystemJob(cron.Job{
		ID: "dream", Name: "dream",
		Schedule: cron.Schedule{Kind: cron.KindEvery, Every: 2 * time.Hour},
		Payload:  cron.Payload{Kind: cron.PayloadSystem},
	}); err != nil {
		t.Fatal(err)
	}

	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Cron: svc})
	client := h.srv.Client()

	resp, err := client.Get(h.srv.URL + CronPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/cron = %d %s", resp.StatusCode, body)
	}
	var list CronList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Jobs) < 2 {
		t.Fatalf("jobs = %+v", list.Jobs)
	}
	if strings.Contains(string(body), "sessionKey") {
		t.Errorf("response leaked sessionKey: %s", body)
	}
	var stretch CronJobView
	for _, j := range list.Jobs {
		if j.ID == job.ID {
			stretch = j
		}
		if j.ID == "dream" && !j.System {
			t.Errorf("dream should be system")
		}
	}
	if stretch.Name != "stretch" || stretch.Schedule.Summary == "" {
		t.Errorf("stretch = %+v", stretch)
	}

	resp, err = client.Post(h.srv.URL+CronPath+"/"+job.ID+"/run", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run = %d %s", resp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+CronPath+"/dream", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete system = %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodDelete, h.srv.URL+CronPath+"/"+job.ID, nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodDelete, h.srv.URL+CronPath+"/"+job.ID, nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing = %d", resp.StatusCode)
	}
}

func TestCronUICreateAndPatch(t *testing.T) {
	svc, err := cron.NewService(cron.ServiceConfig{
		Store: cron.NewStore(t.TempDir()),
		Run:   func(context.Context, cron.Job) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterSystemJob(cron.Job{
		ID: "dream", Name: "dream",
		Schedule: cron.Schedule{Kind: cron.KindEvery, Every: 2 * time.Hour},
		Payload:  cron.Payload{Kind: cron.PayloadSystem},
	}); err != nil {
		t.Fatal(err)
	}

	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Cron: svc})
	client := h.srv.Client()
	origin := h.srv.URL

	// Create every job.
	createBody := `{"name":"water","message":"drink water","schedule":{"kind":"every","everyMs":3600000}}`
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+CronPath, strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var created CronJobView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "water" || created.Schedule.Kind != "every" || created.Schedule.EveryMs != 3600000 {
		t.Fatalf("created = %+v", created)
	}
	if created.Binding == nil || created.Binding.Channel != "websocket" || created.Binding.ChatID != "cron" {
		t.Fatalf("default binding = %+v", created.Binding)
	}
	if strings.Contains(string(body), "sessionKey") {
		t.Errorf("create leaked sessionKey: %s", body)
	}

	// Patch name + disable.
	patchBody := `{"name":"hydrate","enabled":false}`
	req, _ = http.NewRequest(http.MethodPatch, h.srv.URL+CronPath+"/"+created.ID, strings.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	var patched CronJobView
	if err := json.Unmarshal(body, &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Name != "hydrate" || patched.Enabled {
		t.Fatalf("patched = %+v", patched)
	}
	if patched.State.NextRun != "" {
		t.Fatalf("disabled job should clear nextRun, got %q", patched.State.NextRun)
	}

	// Patch schedule to cron expr.
	patchBody = `{"schedule":{"kind":"cron","expr":"0 9 * * 1-5","tz":"Asia/Shanghai"},"enabled":true}`
	req, _ = http.NewRequest(http.MethodPatch, h.srv.URL+CronPath+"/"+created.ID, strings.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch schedule = %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Schedule.Kind != "cron" || patched.Schedule.Expr != "0 9 * * 1-5" || patched.Schedule.TZ != "Asia/Shanghai" {
		t.Fatalf("schedule = %+v", patched.Schedule)
	}
	if !patched.Enabled || patched.State.NextRun == "" {
		t.Fatalf("re-enabled = %+v", patched)
	}

	// System job cannot be patched.
	req, _ = http.NewRequest(http.MethodPatch, h.srv.URL+CronPath+"/dream", strings.NewReader(`{"name":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("patch system = %d", resp.StatusCode)
	}

	// Missing job → 404.
	req, _ = http.NewRequest(http.MethodPatch, h.srv.URL+CronPath+"/missing", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("patch missing = %d", resp.StatusCode)
	}

	// Bad create (no message).
	req, _ = http.NewRequest(http.MethodPost, h.srv.URL+CronPath, strings.NewReader(`{"schedule":{"kind":"every","everyMs":1000}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create missing message = %d", resp.StatusCode)
	}
}

func TestCronUIRuns(t *testing.T) {
	fail, reply := false, ""
	svc, err := cron.NewService(cron.ServiceConfig{
		Store: cron.NewStore(t.TempDir()),
		Run: func(context.Context, cron.Job) (string, error) {
			if fail {
				return "", errors.New("provider down")
			}
			return reply, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.Add(cron.AddSpec{
		Name:     "stretch",
		Message:  "stretch now",
		Schedule: cron.Schedule{Kind: cron.KindEvery, Every: time.Hour},
		Binding:  &cron.Binding{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Cron: svc})
	client := h.srv.Client()
	getRuns := func(id string) (int, []byte) {
		t.Helper()
		resp, err := client.Get(h.srv.URL + CronPath + "/" + id + "/runs")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	code, body := getRuns(job.ID)
	if code != http.StatusOK || !strings.Contains(string(body), `"runs":[]`) {
		t.Fatalf("runs before any run = %d %s", code, body)
	}
	huge := strings.Repeat("长", 50_000)
	for _, r := range []struct {
		fail  bool
		reply string
	}{{false, "Time to stretch!"}, {true, ""}, {false, huge}} {
		fail, reply = r.fail, r.reply
		resp, err := client.Post(h.srv.URL+CronPath+"/"+job.ID+"/run", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("run = %d", resp.StatusCode)
		}
	}
	code, body = getRuns(job.ID)
	if code != http.StatusOK {
		t.Fatalf("runs = %d %s", code, body)
	}
	if strings.Contains(string(body), "sessionKey") || strings.Contains(string(body), "telegram:42") {
		t.Errorf("runs leaked the session: %s", body)
	}
	var v CronRunsView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.JobID != job.ID || v.Name != "stretch" || len(v.Runs) != 3 {
		t.Fatalf("runs = %+v", v)
	}
	// Newest first; the huge reply is truncated.
	if n := len([]rune(v.Runs[0].Reply)); v.Runs[0].Status != "ok" || n == 0 || n > 4000 || !strings.HasPrefix(huge, v.Runs[0].Reply) {
		t.Errorf("huge reply run: status %s, %d runes", v.Runs[0].Status, n)
	}
	if len(body) > 20_000 {
		t.Errorf("runs body is %d bytes", len(body))
	}
	if r := v.Runs[1]; r.Status != "error" || r.Error != "provider down" || r.Reply != "" {
		t.Errorf("failed run = %+v", r)
	}
	if r := v.Runs[2]; r.Status != "ok" || r.Error != "" || r.Reply != "Time to stretch!" {
		t.Errorf("first run = %+v", r)
	}
	if _, err := time.Parse(time.RFC3339, v.Runs[1].RunAt); err != nil {
		t.Errorf("runAt %q: %v", v.Runs[0].RunAt, err)
	}

	if code, _ := getRuns("missing"); code != http.StatusNotFound {
		t.Errorf("runs of a missing job = %d", code)
	}
}

func TestCronUIRunsAuth(t *testing.T) {
	svc, err := cron.NewService(cron.ServiceConfig{Store: cron.NewStore(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.Add(cron.AddSpec{Message: "hi", Schedule: cron.Schedule{Kind: cron.KindEvery, Every: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := New(Config{Token: testToken}, channels.Deps{Bus: bus.NewMessageBus(8)}, Options{Host: "0.0.0.0", Cron: svc})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	ch.Mount(mux)
	target := CronPath + "/" + job.ID + "/runs"
	if rec := do(mux, http.MethodGet, target, lanHost, "192.168.1.9:5555", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("LAN without sign-in: %d", rec.Code)
	}
	if rec := do(mux, http.MethodGet, target, lanHost, "192.168.1.9:5555", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}); rec.Code != http.StatusOK {
		t.Errorf("LAN with token: %d %s", rec.Code, rec.Body)
	}
}
