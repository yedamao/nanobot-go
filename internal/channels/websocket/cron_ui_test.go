package websocket

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/cron"
)

func TestCronUIListRunRemove(t *testing.T) {
	svc, err := cron.NewService(cron.ServiceConfig{
		Store: cron.NewStore(t.TempDir()),
		Run:   func(context.Context, cron.Job) error { return nil },
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
