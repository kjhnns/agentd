package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

// Agent is a second-opinion agent reachable through a GENERIC agent daemon
// API (agentd): one session, the photos uploaded as media turns, the last
// turn carrying the task. Fuel is an ordinary client of that API; nothing
// about Fuel lives in the daemon, and the design does not depend on which
// harness (claude-code, codex, ...) answers.
type Agent interface {
	// Ask runs the task with the photos and returns the agent's final text
	// and a tag naming who answered (for example "agentd:claude-code").
	Ask(ctx context.Context, task string, photos [][]byte) (answer, tag string, err error)
}

// AgentdClient talks to agentd's API: POST /sessions, POST
// /sessions/:id/media (multipart "file" + "text", one image per turn; the
// daemon stores the file where the agent can read it and tells the agent its
// path), DELETE /sessions/:id.
type AgentdClient struct {
	Base   string
	Token  string
	Client *http.Client
}

func (a *AgentdClient) do(ctx context.Context, method, path, ct string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.Base, "/")+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	c := a.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}

func (a *AgentdClient) Ask(ctx context.Context, task string, photos [][]byte) (string, string, error) {
	if len(photos) == 0 {
		return "", "", fmt.Errorf("agent: no photos")
	}
	code, b, err := a.do(ctx, http.MethodPost, "/sessions", "application/json", strings.NewReader(`{"title":"fuel second opinion"}`))
	if err != nil {
		return "", "", err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return "", "", &statusError{"agent session", code}
	}
	var sess struct {
		ID      string `json:"id"`
		Harness string `json:"harness"`
	}
	if json.Unmarshal(b, &sess) != nil || sess.ID == "" {
		return "", "", fmt.Errorf("agent: unreadable session answer")
	}
	tag := "agentd:" + sess.Harness
	// Always tear the session down, also when the job's context is done.
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15e9)
		defer cancel()
		_, _, _ = a.do(dctx, http.MethodDelete, "/sessions/"+sess.ID, "", nil)
	}()
	answer := ""
	for i, p := range photos {
		text := fmt.Sprintf("Photo %d of %d for a task that follows in this session. Do not analyse it yet. Reply only: READY", i+1, len(photos))
		if i == len(photos)-1 {
			text = task
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="photo-%d.jpg"`, i+1))
		hdr.Set("Content-Type", "image/jpeg")
		w, _ := mw.CreatePart(hdr)
		_, _ = w.Write(p)
		_ = mw.WriteField("text", text)
		_ = mw.Close()
		code, b, err := a.do(ctx, http.MethodPost, "/sessions/"+sess.ID+"/media", mw.FormDataContentType(), &buf)
		if err != nil {
			return "", tag, err
		}
		if code != http.StatusOK {
			return "", tag, &statusError{"agent turn", code}
		}
		var turn struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(b, &turn) != nil {
			return "", tag, fmt.Errorf("agent: unreadable turn answer")
		}
		answer = turn.Result
	}
	return answer, tag, nil
}

// AgentModel answers the model step of a chat turn through an agent daemon
// (the generic agentd API) instead of a chat-completions endpoint: the
// system text, the output schema and the labelled data go in as the task,
// photos as media turns. The answer is validated like any model output.
type AgentModel struct {
	Agent *AgentdClient
}

func (a *AgentModel) Estimate(ctx context.Context, in ModelInput) (json.RawMessage, error) {
	schema, _ := json.Marshal(outputSchema())
	task := systemFor(in) + "\n\nAnswer with ONLY one JSON object that matches this JSON schema exactly (every key present, no other text):\n" + string(schema) + "\n\n"
	for _, p := range userContent(in) {
		if t, _ := p["text"].(string); t != "" {
			task += t
		}
	}
	photos := append(append([][]byte{}, in.Images...), in.RefImages...)
	var answer string
	var err error
	if len(photos) == 0 {
		answer, err = a.Agent.AskText(ctx, task)
	} else {
		task += "\nThe photo(s) are the image files saved in this session; view every one before you answer."
		answer, _, err = a.Agent.Ask(ctx, task, photos)
	}
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(answer)
	if i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); i >= 0 && j > i {
		raw = raw[i : j+1]
	}
	return json.RawMessage(raw), nil
}

// AskText runs a text-only task in a fresh session (POST /sessions, POST
// /sessions/:id/input, DELETE).
func (a *AgentdClient) AskText(ctx context.Context, task string) (string, error) {
	code, b, err := a.do(ctx, http.MethodPost, "/sessions", "application/json", strings.NewReader(`{"title":"fuel chat turn"}`))
	if err != nil {
		return "", err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return "", &statusError{"agent session", code}
	}
	var sess struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &sess) != nil || sess.ID == "" {
		return "", fmt.Errorf("agent: unreadable session answer")
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15e9)
		defer cancel()
		_, _, _ = a.do(dctx, http.MethodDelete, "/sessions/"+sess.ID, "", nil)
	}()
	body, _ := json.Marshal(map[string]string{"text": task})
	code, b, err = a.do(ctx, http.MethodPost, "/sessions/"+sess.ID+"/input", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", &statusError{"agent turn", code}
	}
	var turn struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(b, &turn) != nil {
		return "", fmt.Errorf("agent: unreadable turn answer")
	}
	return turn.Result, nil
}
