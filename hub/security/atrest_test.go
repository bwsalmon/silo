package security_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// TestNoCodeAtRest drives a task through a fake node that sends what
// only its node may keep (SILO_ONLY: diffs, file contents, tool
// arguments and results, the assistant's streamed text), in every
// shape the event schema allows and some it does not, and serves a
// diff through the view-through. Then it reads every row of every
// table of the hub's database, and every line the hub logged, and
// requires that none of it holds any of that content. The artifact the
// view-through passed on must also be marked no-store.
func TestNoCodeAtRest(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { noCodeAtRest(t, "") })
	// pgtest.Start skips where there is no PostgreSQL, unless CI
	// requires one (SILO_REQUIRE_POSTGRES).
	t.Run("postgres", func(t *testing.T) { noCodeAtRest(t, pgtest.Start(t).URL) })
}

func sentinel(kind string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return "SILOONLY" + strings.ToUpper(kind) + hex.EncodeToString(b)
}

type memoryRepoFetcher map[string][]byte

func (m memoryRepoFetcher) FetchFile(_ context.Context, repoURL, ref, filePath string) ([]byte, error) {
	if b, ok := m[repoURL+"|"+filePath+"|"+ref]; ok {
		return b, nil
	}
	if b, ok := m[repoURL+"|"+filePath]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("not found: %s %s @ %s", repoURL, filePath, ref)
}

func signWebhook(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func noCodeAtRest(t *testing.T, database string) {
	var (
		diff          = sentinel("diff")
		file          = sentinel("file")
		toolArg       = sentinel("toolarg")
		toolOut       = sentinel("toolout")
		thinking      = sentinel("token")
		smuggled      = sentinel("smuggled")
		pathOnly      = sentinel("path")
		msgText       = sentinel("message")
		grainfileBody = sentinel("grainfilebody")
		refContent    = sentinel("refcontent")
	)
	const gfCommit = "1111111111111111111111111111111111111111"
	gfRaw := fmt.Sprintf("---\ntitle: Migrate App Database\nrecipient: https://github.com/acme/grain-a\nreferences:\n  - /docs/spec.md\n---\n%s\n", grainfileBody)
	fetcher := memoryRepoFetcher{
		"https://github.com/acme/app|.grain/tasks/migrate.task.md|" + gfCommit: []byte(gfRaw),
		"https://github.com/acme/app|docs/spec.md|" + gfCommit:                 []byte(refContent),
	}

	hb := startHub(t, options{database: database, grainfileFetcher: fetcher})
	hb.teams()
	tok, _ := hb.mint(alice, "ws-a")
	n := connectNode(t, hb, tok, "ws-a", repoA)
	if _, err := hb.h.Grainfiles.SetSubscriptions(context.Background(), grainfiles.Subscriptions{
		NodeID:     "ws-a",
		Owner:      alice,
		Recipients: []string{"https://github.com/acme/grain-a"},
	}); err != nil {
		t.Fatal(err)
	}

	// Ingest a Grainfile via signed GitHub push webhook and fetch both the
	// Grainfile and its referenced file through GET /v1/grainfiles/content.
	whPayload, _ := json.Marshal(map[string]any{
		"ref":   "refs/heads/main",
		"after": gfCommit,
		"repository": map[string]any{
			"html_url": "https://github.com/acme/app",
		},
		"commits": []map[string]any{
			{
				"id":    gfCommit,
				"added": []string{".grain/tasks/migrate.task.md"},
			},
		},
	})
	whReq, _ := http.NewRequest("POST", hb.url+grainfiles.GitHubWebhookPath, bytes.NewReader(whPayload))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-GitHub-Event", "push")
	whReq.Header.Set("X-Hub-Signature-256", signWebhook(webhookSecret, whPayload))
	whResp, err := http.DefaultClient.Do(whReq)
	if err != nil {
		t.Fatal(err)
	}
	whResp.Body.Close()
	if whResp.StatusCode != http.StatusOK {
		t.Fatalf("webhook ingestion: %d, want 200", whResp.StatusCode)
	}

	gfURL := "https://github.com/acme/app/blob/main/.grain/tasks/migrate.task.md"
	gfCode, gfBytes, gfHdr := hb.do(bearer(tok), "GET", grainfiles.ContentPath+"?grainfile_url="+gfURL, nil)
	if gfCode != http.StatusOK || !strings.Contains(string(gfBytes), grainfileBody) {
		t.Fatalf("proxying grainfile body: %d %q", gfCode, gfBytes)
	}
	if cc := gfHdr.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("grainfile content Cache-Control = %q, want no-store", cc)
	}
	if nosniff := gfHdr.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("grainfile content X-Content-Type-Options = %q, want nosniff", nosniff)
	}
	refCode, refBytes, refHdr := hb.do(bearer(tok), "GET", grainfiles.ContentPath+"?grainfile_url="+gfURL+"&reference=/docs/spec.md", nil)
	if refCode != http.StatusOK || !strings.Contains(string(refBytes), refContent) {
		t.Fatalf("proxying reference file: %d %q", refCode, refBytes)
	}
	if cc := refHdr.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("reference content Cache-Control = %q, want no-store", cc)
	}

	w := &world{hb: hb}
	task := w.run(alice, repoA, n, true)
	n.mu.Lock()
	n.artifact = "--- a/main.go\n+++ b/main.go\n+" + diff + "\n"
	n.mu.Unlock()
	id := task.ID
	lines := []string{
		// Node-only by type: the hub keeps a reference, not the payload.
		event(id, 1, events.TypeToolCall, fmt.Sprintf(`{"phase":"start","call":1,"tool":"bash","args":{"cmd":"cat main.go","stdin":%q}}`, toolArg)),
		event(id, 2, events.TypeToolCall, fmt.Sprintf(`{"phase":"end","call":1,"tool":"bash","result":%q}`, toolOut)),
		event(id, 3, events.TypeTranscript, fmt.Sprintf(`{"kind":"tool_use","id":"tu-1","tool":"edit","args":{"path":"main.go","new":%q}}`, file)),
		event(id, 4, events.TypeTranscript, fmt.Sprintf(`{"kind":"tool_result","id":"tu-1","text":%q}`, file)),
		event(id, 5, live.TypeArtifact, fmt.Sprintf(`{"kind":"diff","ref":"diff-1","size":40,"patch":%q}`, diff)),
		event(id, 6, "file", fmt.Sprintf(`{"path":"main.go","content":%q}`, file)),
		// Token: broadcast, held in memory, never stored.
		event(id, 7, events.TypeTranscript, fmt.Sprintf(`{"kind":"text","text":%q}`, thinking)),
		// Stored types carrying what they may not: rejected whole.
		event(id, 8, events.TypeStatus, fmt.Sprintf(`{"phase":"running","state":"info","message":"ok","diff":%q}`, smuggled)),
		fmt.Sprintf(`{"v":1,"seq":9,"type":"status","task":%q,"status":{"phase":"running","state":"info","message":"ok"},"file":%q}`, id, smuggled),
		event(id, 10, events.TypeTranscript, fmt.Sprintf(`{"kind":"text","text":"ok","args":{"x":%q}}`, smuggled)),
		// A message the node claims a human sent: the hub keeps only
		// its own record, by ID.
		event(id, 11, events.TypeMessage, fmt.Sprintf(`{"id":"m-none","text":%q}`, msgText)),
		// Paths on the node's disk, in a result: dropped.
		event(id, 12, events.TypeResult, fmt.Sprintf(`{"state":"done","audit_path":%q,"transcript_path":%q}`, pathOnly, pathOnly)),
	}
	up := n.events(lines...)

	// A subscriber sees references, and the token text live, but none
	// of the node-only payloads.
	_, seen := hb.replay(hb.user(alice), id, 0, 7)
	for _, e := range seen {
		b := fmt.Sprintf("%+v %+v %+v %+v", e.ToolCall, e.Transcript, e.Artifact, e.Status)
		for _, s := range []string{diff, file, toolArg, toolOut, smuggled} {
			if strings.Contains(b, s) {
				t.Errorf("a subscriber was sent node-only content in event %d (%s)", e.Seq, e.Type)
			}
		}
	}

	// The diff, through the view-through: passed on, never cached.
	eventually(t, "the diff listed", func() bool {
		code, _, _ := hb.do(hb.user(alice), "GET", tasks.TasksPath+"/"+id+"/artifacts?ref=diff-1", nil)
		return code == http.StatusOK
	})
	code, body, h := hb.do(hb.user(dave), "GET", tasks.TasksPath+"/"+id+"/artifacts?ref=diff-1", nil)
	if code != http.StatusOK || !strings.Contains(string(body), diff) {
		t.Fatalf("viewing the diff: %d %q", code, body)
	}
	if cc := h.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("the artifact's Cache-Control is %q, want no-store", cc)
	}

	// The task ends.
	up.Close()
	st := n.taskStream(id)
	st.write(tasks.Message{Type: tasks.TypeResult, State: tasks.Done, Reason: "finished"})
	eventually(t, "the task done", func() bool {
		got, _ := hb.task(hb.user(alice), id)
		return got.State == tasks.Done
	})
	// Let the hub finish writing what it keeps.
	time.Sleep(500 * time.Millisecond)

	// The dump sees what the hub does keep: the prompt, and the diff's
	// reference.
	dumped := dump(t, hb.db)
	var whole strings.Builder
	for _, rows := range dumped {
		whole.WriteString(strings.Join(rows, "\n"))
	}
	if !strings.Contains(whole.String(), "do the thing") || !strings.Contains(whole.String(), "diff-1") || !strings.Contains(whole.String(), "Migrate App Database") {
		t.Fatal("the dump holds neither the task's prompt, the diff's reference, nor the grainfile's front-matter title: it is not reading the hub's rows")
	}
	all := []string{diff, file, toolArg, toolOut, thinking, smuggled, pathOnly, msgText, grainfileBody, refContent}
	for table, rows := range dumped {
		for _, row := range rows {
			for _, s := range all {
				if strings.Contains(row, s) {
					t.Errorf("table %s holds SILO_ONLY content %s: %.300s", table, s[:len("SILOONLY")+8], row)
				}
			}
		}
	}
	logged := hb.logs.all()
	for _, s := range all {
		if strings.Contains(logged, s) {
			t.Errorf("the hub logged SILO_ONLY content %s", s)
		}
	}
	if !strings.Contains(logged, "refused") && !strings.Contains(logged, "outside its schema") {
		t.Log("note: the hub logged no rejection; the smuggled events may not have been read")
	}
}

// dump reads every row of every table of the hub's database, each row
// as one string of all its columns.
func dump(t *testing.T, database string) map[string][]string {
	t.Helper()
	var (
		db     *sql.DB
		err    error
		tables string
	)
	if path, ok := strings.CutPrefix(database, "sqlite:"); ok {
		db, err = sql.Open("sqlite", "file:"+path)
		tables = `SELECT name FROM sqlite_master WHERE type = 'table'`
	} else {
		db, err = sql.Open("pgx", database)
		tables = `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, tables)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if len(names) < 5 {
		t.Fatalf("the database has only tables %q: not the hub's", names)
	}
	out := map[string][]string{}
	for _, n := range names {
		rs, err := db.QueryContext(ctx, `SELECT * FROM "`+n+`"`)
		if err != nil {
			t.Fatalf("reading table %s: %v", n, err)
		}
		cols, _ := rs.Columns()
		out[n] = []string{}
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatalf("reading table %s: %v", n, err)
			}
			var sb strings.Builder
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				fmt.Fprintf(&sb, "%s=%v ", cols[i], v)
			}
			out[n] = append(out[n], sb.String())
		}
		rs.Close()
	}
	return out
}
