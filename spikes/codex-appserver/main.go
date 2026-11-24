// Command codex-appserver is spike M1.1 (docs/architecture.md §17.1) plus the Codex part
// of M1.2 (stdin EOF) and M1.5 (structured output). It drives `codex app-server` over
// JSON-RPC and records what actually happens.
//
// Usage: go run ./codex-appserver -out <dir> [-model gpt-6-luna]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ugurboz/openchef/spikes/internal/mcpserver"
	"github.com/ugurboz/openchef/spikes/internal/rpc"
)

type approval struct {
	Method   string          `json:"method"`
	Decision string          `json:"decision"`
	Params   json.RawMessage `json:"params"`
}

type turnResult struct {
	Status    string          `json:"status"`
	Error     json.RawMessage `json:"error,omitempty"`
	FinalText string          `json:"finalText"`
	ItemTypes []string        `json:"itemTypes"`
	Elapsed   string          `json:"elapsed"`
}

type findings struct {
	Version      string                `json:"codexVersion"`
	Model        string                `json:"model"`
	RateLimits   json.RawMessage       `json:"rateLimitsRead,omitempty"`
	RateLimitsUp int                   `json:"rateLimitsUpdatedNotifications"`
	Turns        map[string]turnResult `json:"turns"`
	Approvals    []approval            `json:"approvals"`
	UserInput    []json.RawMessage     `json:"requestUserInput"`
	Checks       map[string]any        `json:"checks"`
	Errors       []string              `json:"errors"`
}

type spike struct {
	out, work, model string
	mu               sync.Mutex
	f                findings
	decision         string // decision for the next approvals
	turnID           string // id of the turn in flight (for turn/interrupt)
}

func main() {
	out := flag.String("out", "", "output directory for transcripts and findings")
	model := flag.String("model", "gpt-6-luna", "cheapest model to use")
	mcpServer := flag.Bool("mcp-server", false, "internal: run as a minimal MCP server exposing ask_user")
	mcpLog := flag.String("mcp-log", "", "internal: file that records MCP tool calls")
	flag.Parse()
	if *mcpServer {
		if err := mcpserver.Serve(*mcpLog); err != nil {
			fmt.Fprintln(os.Stderr, "mcp server:", err)
			os.Exit(1)
		}
		return
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	if err := run(*out, *model); err != nil {
		fmt.Fprintln(os.Stderr, "spike failed:", err)
		os.Exit(1)
	}
}

func run(out, model string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "openchef-codex-spike-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	repo := filepath.Join(work, "repo")
	if err := rpc.NewGitRepo(repo); err != nil {
		return err
	}
	s := &spike{out: out, work: work, model: model, decision: "accept"}
	s.f.Turns = map[string]turnResult{}
	s.f.Checks = map[string]any{}
	s.f.Model = model

	s.mainScenario(repo)
	s.eofScenarios(repo)
	s.mcpScenario(repo)

	raw, _ := json.MarshalIndent(s.f, "", "  ")
	scrubber, err := rpc.NewTranscript(filepath.Join(work, "scrub.jsonl"), work)
	if err != nil {
		return err
	}
	defer scrubber.Close()
	return os.WriteFile(filepath.Join(out, "codex-findings.json"), scrubber.ScrubJSON(raw), 0o644)
}

func (s *spike) errf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	rpc.Logf("ERROR: %s", msg)
	s.mu.Lock()
	s.f.Errors = append(s.f.Errors, msg)
	s.mu.Unlock()
}

func (s *spike) handler(method string, params json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval",
		"execCommandApproval", "applyPatchApproval":
		s.f.Approvals = append(s.f.Approvals, approval{Method: method, Decision: s.decision, Params: params})
		return map[string]any{"decision": s.decision}, nil
	case "mcpServer/elicitation/request":
		// Codex asks before running an MCP tool. OpenChef auto-allows its own ask_user
		// tool (same pattern as Claude's permission prompt for mcp__openchef__ask_user).
		s.f.Approvals = append(s.f.Approvals, approval{Method: method, Decision: "accept", Params: params})
		return map[string]any{"action": "accept", "content": map[string]any{}}, nil
	case "item/tool/requestUserInput":
		s.f.UserInput = append(s.f.UserInput, params)
		var p struct {
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(params, &p)
		answers := map[string]any{}
		for _, q := range p.Questions {
			answers[q.ID] = map[string]any{"answers": []string{"teal"}}
		}
		return map[string]any{"answers": answers}, nil
	default:
		return nil, fmt.Errorf("spike does not handle %s", method)
	}
}

func (s *spike) start(repo, name string) (*rpc.Client, *rpc.Transcript, error) {
	tr, err := rpc.NewTranscript(filepath.Join(s.out, "codex-"+name+".jsonl"), s.work)
	if err != nil {
		return nil, nil, err
	}
	c, err := rpc.Start("codex", []string{"app-server"}, repo, tr, s.handler)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var init json.RawMessage
	if err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "openchef-spike", "version": "0.0.0"},
	}, &init); err != nil {
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}
	if err := c.Notify("initialized", nil); err != nil {
		return nil, nil, err
	}
	return c, tr, nil
}

func (s *spike) threadStart(c *rpc.Client, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	err := c.Call(ctx, "thread/start", map[string]any{
		"cwd":            repo,
		"model":          s.model,
		"approvalPolicy": "untrusted",
		"sandbox":        "workspace-write",
		"ephemeral":      false,
	}, &res)
	if err != nil {
		return "", err
	}
	if res.Thread.ID == "" {
		return "", errors.New("thread/start returned no thread id")
	}
	return res.Thread.ID, nil
}

// turn runs one turn and waits for turn/completed.
func (s *spike) turn(c *rpc.Client, thread, name, text string, extra map[string]any, onDelta func()) turnResult {
	params := map[string]any{
		"threadId": thread,
		"input":    []any{map[string]any{"type": "text", "text": text}},
		"effort":   "low",
	}
	for k, v := range extra {
		params[k] = v
	}
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := c.Call(ctx, "turn/start", params, &started); err != nil {
		s.errf("%s: turn/start: %v", name, err)
		return turnResult{Status: "start-failed"}
	}
	s.mu.Lock()
	s.turnID = started.Turn.ID
	s.mu.Unlock()
	res := turnResult{}
	_, err := c.Await(4*time.Minute, func(m rpc.Message) bool {
		switch m.Method {
		case "account/rateLimits/updated":
			s.mu.Lock()
			s.f.RateLimitsUp++
			s.mu.Unlock()
		case "item/agentMessage/delta":
			if onDelta != nil {
				onDelta()
				onDelta = nil
			}
		case "item/completed":
			var p struct {
				Item map[string]any `json:"item"`
			}
			_ = json.Unmarshal(m.Params, &p)
			typ, _ := p.Item["type"].(string)
			res.ItemTypes = append(res.ItemTypes, typ)
			if typ == "agentMessage" {
				if txt, ok := p.Item["text"].(string); ok {
					res.FinalText = txt
				}
			}
		case "turn/completed":
			var p struct {
				Turn struct {
					Status string          `json:"status"`
					Error  json.RawMessage `json:"error"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(m.Params, &p)
			res.Status = p.Turn.Status
			res.Error = p.Turn.Error
			return true
		}
		return false
	})
	if err != nil {
		s.errf("%s: waiting for turn/completed: %v", name, err)
		res.Status = "no-completion"
	}
	res.Elapsed = time.Since(t0).Round(time.Millisecond).String()
	s.mu.Lock()
	s.f.Turns[name] = res
	s.mu.Unlock()
	rpc.Logf("turn %-14s status=%s items=%v text=%.80q (%s)", name, res.Status, res.ItemTypes, res.FinalText, res.Elapsed)
	return res
}

func (s *spike) mainScenario(repo string) {
	c, tr, err := s.start(repo, "main")
	if err != nil {
		s.errf("start: %v", err)
		return
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	var limits json.RawMessage
	if err := c.Call(ctx, "account/rateLimits/read", nil, &limits); err != nil {
		s.errf("rateLimits/read: %v", err)
	} else {
		var v any
		_ = json.Unmarshal(limits, &v)
		s.f.RateLimits, _ = json.Marshal(redactForFindings(v))
	}
	cancel()

	thread, err := s.threadStart(c, repo)
	if err != nil {
		s.errf("thread/start: %v", err)
		return
	}

	// 1. Command approval accepted.
	s.setDecision("accept")
	s.turn(c, thread, "approve-cmd",
		"Remember the code word HERON for later. Now run exactly this shell command: echo spike > out.txt   Then reply with the single word DONE.", nil, nil)
	_, statErr := os.Stat(filepath.Join(repo, "out.txt"))
	s.f.Checks["approvedCommandCreatedFile"] = statErr == nil

	// 2. Command approval declined.
	s.setDecision("decline")
	s.turn(c, thread, "decline-cmd",
		"Run exactly this shell command: echo denied > denied.txt   Then reply with the single word DONE.", nil, nil)
	_, statErr = os.Stat(filepath.Join(repo, "denied.txt"))
	s.f.Checks["declinedCommandCreatedFile"] = statErr == nil
	s.setDecision("accept")

	// 3. Structured output.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string"},
			"steps": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []string{"title", "steps"},
		"additionalProperties": false,
	}
	r := s.turn(c, thread, "output-schema",
		"Plan how to add a hello-world shell script to this repository in exactly 2 steps. Do not change any files.",
		map[string]any{"outputSchema": schema}, nil)
	var parsed struct {
		Title string   `json:"title"`
		Steps []string `json:"steps"`
	}
	s.f.Checks["outputSchemaValidJSON"] = json.Unmarshal([]byte(r.FinalText), &parsed) == nil && len(parsed.Steps) > 0

	// 4. User input request.
	r = s.turn(c, thread, "user-input",
		"Before doing anything else, ask me which colour I prefer using your request_user_input tool (options: teal, amber). Then reply with exactly the colour I chose and nothing else.", nil, nil)
	s.f.Checks["requestUserInputReceived"] = len(s.f.UserInput) > 0
	s.f.Checks["userInputAnswerUsed"] = strings.Contains(strings.ToLower(r.FinalText), "teal")

	// 5. Interrupt mid-turn.
	var once sync.Once
	r = s.turn(c, thread, "interrupt",
		"Write a 300-word story about a lighthouse keeper. Do not run any commands.",
		nil, func() {
			once.Do(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := c.Call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": s.currentTurn()}, nil); err != nil {
					s.errf("turn/interrupt: %v", err)
				}
			})
		})
	s.f.Checks["interruptStatus"] = r.Status

	// 6. Kill -9 and resume in a new process.
	if err := c.Kill(); err != nil {
		s.errf("kill: %v", err)
	}
	c.WaitExit(10 * time.Second)

	c2, tr2, err := s.start(repo, "resume")
	if err != nil {
		s.errf("restart: %v", err)
		return
	}
	defer tr2.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
	err = c2.Call(ctx, "thread/resume", map[string]any{"threadId": thread, "excludeTurns": true}, nil)
	cancel()
	if err != nil {
		s.errf("thread/resume: %v", err)
	} else {
		r = s.turn(c2, thread, "after-resume", "What was the code word I asked you to remember? Reply with just the word.", nil, nil)
		s.f.Checks["resumeRecalledCodeWord"] = strings.Contains(strings.ToUpper(r.FinalText), "HERON")
	}
	_ = c2.CloseStdin()
	if d, ok := c2.WaitExit(15 * time.Second); ok {
		s.f.Checks["exitAfterEOFIdle"] = d.Round(time.Millisecond).String()
	} else {
		s.f.Checks["exitAfterEOFIdle"] = "did not exit within 15s"
		_ = c2.Kill()
	}
}

// eofScenarios: does app-server exit when stdin closes in the middle of a turn?
func (s *spike) eofScenarios(repo string) {
	c, tr, err := s.start(repo, "eof-midturn")
	if err != nil {
		s.errf("start eof: %v", err)
		return
	}
	defer tr.Close()
	thread, err := s.threadStart(c, repo)
	if err != nil {
		s.errf("thread/start eof: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Call(ctx, "turn/start", map[string]any{
		"threadId": thread,
		"input":    []any{map[string]any{"type": "text", "text": "Write a 300-word story about a lighthouse keeper. Do not run any commands."}},
		"effort":   "low",
	}, nil); err != nil {
		s.errf("eof turn/start: %v", err)
		return
	}
	if _, err := c.Await(2*time.Minute, func(m rpc.Message) bool { return m.Method == "item/agentMessage/delta" }); err != nil {
		s.errf("eof: no delta: %v", err)
	}
	tr.Note("note", "closing stdin mid-turn")
	_ = c.CloseStdin()
	if d, ok := c.WaitExit(30 * time.Second); ok {
		s.f.Checks["exitAfterEOFMidTurn"] = d.Round(time.Millisecond).String()
	} else {
		s.f.Checks["exitAfterEOFMidTurn"] = "did not exit within 30s"
		_ = c.Kill()
	}
}

func (s *spike) setDecision(d string) {
	s.mu.Lock()
	s.decision = d
	s.mu.Unlock()
}

func (s *spike) currentTurn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnID
}

// redactForFindings drops account identifiers from data copied into findings.
func redactForFindings(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if strings.Contains(strings.ToLower(k), "account") && strings.HasSuffix(strings.ToLower(k), "id") {
				x[k] = "REDACTED"
				continue
			}
			x[k] = redactForFindings(val)
		}
	case []any:
		for i := range x {
			x[i] = redactForFindings(x[i])
		}
	}
	return v
}
