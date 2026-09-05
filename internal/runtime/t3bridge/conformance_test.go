package t3bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gorilla/websocket"
)

// TestMain points HOME/T3_HOME and the bridge state dir at throwaway
// directories for the whole package test run. NewProvider() removes the legacy
// $HOME/.t3/gc-bridge state dir on construction, so a conformance test that
// builds real providers via NewSeamBacked must never run against a developer's
// real HOME. Individual tests that need specific values still override these
// with t.Setenv (which restores them afterwards).
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "t3bridge-test-home")
	if err != nil {
		panic(err)
	}
	stateDir, err := os.MkdirTemp("", "t3bridge-test-state")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("T3_HOME", filepath.Join(home, ".t3"))
	_ = os.Setenv("T3_BEARER_TOKEN", "")
	_ = os.Setenv("GC_T3BRIDGE_STATE_DIR", stateDir)

	code := m.Run()

	_ = os.RemoveAll(home)
	_ = os.RemoveAll(stateDir)
	os.Exit(code)
}

// statefulT3Server is an in-process fake of the T3 orchestration WebSocket API
// that REFLECTS dispatched commands into snapshot state, unlike the static
// test server in provider_test.go. thread.create adds a running thread,
// thread.meta.update merges its metadata, thread.session.stop pauses it (the
// session stops running but the thread stays listable), and thread.archive
// removes it. That state machine is what lets the real t3bridge provider's
// IsRunning / ListRunning / ProcessAlive / Stop contracts execute against
// realistic bridge behavior in the shared runtime conformance suite.
type statefulT3Server struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	projects map[string]map[string]interface{}
	threads  map[string]map[string]interface{}
}

func newStatefulT3Server(t *testing.T) *statefulT3Server {
	t.Helper()
	s := &statefulT3Server{
		t:        t,
		projects: make(map[string]map[string]interface{}),
		threads:  make(map[string]map[string]interface{}),
	}
	upgrader := websocket.Upgrader{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			var req struct {
				ID      string          `json:"id"`
				Tag     string          `json:"tag"`
				Payload json.RawMessage `json:"payload"`
			}
			// A read error here is the provider closing the connection after it
			// received its response (one request per connection); treat it as a
			// normal end of the exchange, not a test failure.
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			value := s.handle(req.Tag, req.Payload)
			resp := map[string]interface{}{
				"_tag":      "Exit",
				"requestId": req.ID,
				"exit": map[string]interface{}{
					"_tag":  "Success",
					"value": value,
				},
			}
			if err := conn.WriteJSON(resp); err != nil {
				return
			}
		}
	}))
	return s
}

func (s *statefulT3Server) Close() { s.server.Close() }

func (s *statefulT3Server) wsURL() string {
	return "ws" + strings.TrimPrefix(s.server.URL, "http")
}

// handle applies one RPC and returns its result value.
func (s *statefulT3Server) handle(tag string, payload json.RawMessage) map[string]interface{} {
	switch tag {
	case "orchestration.getSnapshot":
		return s.snapshot()
	case "orchestration.dispatchCommand":
		s.dispatch(payload)
		return map[string]interface{}{"ok": true}
	default:
		// Unknown methods (e.g. gc.peekThreadMessages) return an empty result;
		// the provider falls back to its snapshot-derived behavior.
		return map[string]interface{}{}
	}
}

func (s *statefulT3Server) snapshot() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	projects := make([]interface{}, 0, len(s.projects))
	for _, p := range s.projects {
		projects = append(projects, cloneJSONMap(p))
	}
	threads := make([]interface{}, 0, len(s.threads))
	for _, th := range s.threads {
		threads = append(threads, cloneJSONMap(th))
	}
	return map[string]interface{}{
		"projects": projects,
		"threads":  threads,
	}
}

// dispatch mutates snapshot state for one command. The command is either the
// payload itself (top-level "type") or nested under "command".
func (s *statefulT3Server) dispatch(payload json.RawMessage) {
	var raw map[string]interface{}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return
	}
	command := raw
	if nested, ok := raw["command"].(map[string]interface{}); ok {
		command = nested
	}
	cmdType, _ := command["type"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	switch cmdType {
	case "project.create":
		id, _ := command["projectId"].(string)
		if id == "" {
			return
		}
		s.projects[id] = map[string]interface{}{
			"id":            id,
			"workspaceRoot": command["workspaceRoot"],
			"title":         command["title"],
			"createdAt":     now,
			"updatedAt":     now,
		}
	case "thread.create":
		id, _ := command["threadId"].(string)
		if id == "" {
			return
		}
		thread := map[string]interface{}{
			"id":        id,
			"projectId": command["projectId"],
			"session":   map[string]interface{}{"status": "running"},
			"createdAt": now,
			"updatedAt": now,
		}
		if meta, ok := command["customMetadata"].(map[string]interface{}); ok {
			thread["customMetadata"] = cloneJSONMap(meta)
		} else {
			thread["customMetadata"] = map[string]interface{}{}
		}
		if branch, ok := command["branch"].(string); ok && branch != "" {
			thread["branch"] = branch
		}
		if wt, ok := command["worktreePath"].(string); ok && wt != "" {
			thread["worktreePath"] = wt
		}
		s.threads[id] = thread
	case "thread.meta.update":
		id, _ := command["threadId"].(string)
		thread := s.threads[id]
		if thread == nil {
			return
		}
		if incoming, ok := command["customMetadata"].(map[string]interface{}); ok {
			meta, _ := thread["customMetadata"].(map[string]interface{})
			if meta == nil {
				meta = map[string]interface{}{}
			}
			for k, v := range incoming {
				meta[k] = v
			}
			thread["customMetadata"] = meta
		}
		if title, ok := command["title"].(string); ok && title != "" {
			thread["title"] = title
		}
		if wt, ok := command["worktreePath"].(string); ok && wt != "" {
			thread["worktreePath"] = wt
		}
		thread["updatedAt"] = now
	case "thread.session.stop":
		id, _ := command["threadId"].(string)
		thread := s.threads[id]
		if thread == nil {
			return
		}
		// Pause, don't delete: the session stops running but the thread stays a
		// listable record until it is archived.
		thread["session"] = map[string]interface{}{"status": "stopped"}
		thread["updatedAt"] = now
	case "thread.archive":
		id, _ := command["threadId"].(string)
		thread := s.threads[id]
		if thread == nil {
			return
		}
		thread["deletedAt"] = now
		thread["updatedAt"] = now
	default:
		// thread.turn.start / thread.turn.interrupt / thread.activity.append and
		// any other command carry no snapshot state the provider reads back.
	}
}

func cloneJSONMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		if nested, ok := v.(map[string]interface{}); ok {
			out[k] = cloneJSONMap(nested)
			continue
		}
		out[k] = v
	}
	return out
}

var (
	t3ConformanceMu      sync.Mutex
	t3ConformanceServers = map[*testing.T]*statefulT3Server{}
	t3ConformanceCounter int64
)

// t3ConformanceConfig ensures a stateful T3 fake server bound to caseT, points
// the bridge's WebSocket env at it, and returns a Config whose startup envelope
// marks the session as a stoppable worker — a bead assignment means the
// provider treats it as ephemeral (not a persistent agent that Stop leaves
// running). The envelope leaves gc.sessionName empty on purpose: the provider
// defaults it to the runtime session name (the factory's third return value),
// so the config never needs to know the name.
//
// The server is created once per caseT and reused for every session that
// subtest starts, so discovery contracts (ListRunning across two sessions,
// concurrent starts) observe one consistent snapshot.
func t3ConformanceConfig(caseT *testing.T) runtime.Config {
	caseT.Helper()

	t3ConformanceMu.Lock()
	srv := t3ConformanceServers[caseT]
	if srv == nil {
		srv = newStatefulT3Server(caseT)
		t3ConformanceServers[caseT] = srv
		caseT.Cleanup(func() {
			srv.Close()
			t3ConformanceMu.Lock()
			delete(t3ConformanceServers, caseT)
			t3ConformanceMu.Unlock()
		})
	}
	t3ConformanceMu.Unlock()

	caseT.Setenv("T3_WS_URL", srv.wsURL())

	envelope := StartupEnvelope{
		Version: 1,
		GC: GCSection{
			CityName: "conform-city",
			Agent:    "conform/worker",
			Template: "conform",
		},
		Runtime: RuntimeSection{
			Provider: "claudeAgent",
			Model:    "claude-sonnet-4-6",
			WorkDir:  caseT.TempDir(),
		},
		Assignment: AssignmentSection{
			BeadID:    "conform-bead",
			BeadTitle: "conformance work",
		},
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		caseT.Fatalf("marshal conformance envelope: %v", err)
	}
	return runtime.Config{
		WorkDir: envelope.Runtime.WorkDir,
		Env: map[string]string{
			"GC_STARTUP_ENVELOPE": string(data),
		},
	}
}

// TestT3BridgeConformance proves internal/runtime/t3bridge.NewSeamBacked against
// the shared runtime.Provider conformance suite, backed by a stateful in-process
// T3 fake. It uses the durable-thread runner because a T3 session is a durable
// thread: Start reuses an existing thread (idempotent) and Stop pauses rather
// than deletes it. This is the provider-ledger proof for both the
// runtime.builtin.t3bridge (exact:) and runtime.builtin.exec (legacy prefix:)
// bindings of NewSeamBacked; keep it in lockstep with those ledger claims.
func TestT3BridgeConformance(t *testing.T) {
	runtimetest.RunDurableThreadProviderTests(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(), t3ConformanceConfig(caseT), fmt.Sprintf("t3-conform-%06d", atomic.AddInt64(&t3ConformanceCounter, 1))
	})
}
