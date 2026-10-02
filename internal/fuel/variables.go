package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Value is one Variables value as the ext API returns it.
type Value struct {
	ID         string
	VariableID string
	RecordDate string
	CreatedAt  time.Time
	Raw        string         // data as stored (a JSON string for json variables)
	Data       map[string]any // parsed json object; nil for non-json values
}

// VarInfo is one variable from GET /variables.
type VarInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Variables is the slice of the ext API the fast path needs. The HTTP client
// implements it; tests inject the same client pointed at a loopback fake.
type Variables interface {
	ListVariables(ctx context.Context) ([]VarInfo, error)
	Post(ctx context.Context, variableID string, data map[string]any, recordDate string) (string, error)
	ByDate(ctx context.Context, date string) ([]Value, error)
	All(ctx context.Context) ([]Value, error)
}

// PostError is a definitive (non-retryable) rejection of a write: the value
// was NOT stored. Anything else from Post is uncertain.
type PostError struct {
	Status int
	Body   string
}

// Error never includes the body: logs must not carry upstream text.
func (e *PostError) Error() string { return fmt.Sprintf("variables: HTTP %d", e.Status) }

// statusError is a non-2xx answer, reported by status only.
type statusError struct {
	What   string
	Status int
}

func (e *statusError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.What, e.Status) }

// VariablesHTTP talks to {base}/variables and {base}/values.
type VariablesHTTP struct {
	Base   string
	Key    string
	Client *http.Client
}

func (v *VariablesHTTP) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(v.Base, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+v.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := v.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

func (v *VariablesHTTP) ListVariables(ctx context.Context) ([]VarInfo, error) {
	code, b, err := v.do(ctx, http.MethodGet, "/variables?includeJson=true", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, &statusError{"variables list", code}
	}
	var out []VarInfo
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("variables list: %w", err)
	}
	return out, nil
}

// Post writes one json value. 201 returns the value id. A 4xx other than
// 408/429 is a *PostError (definitive); every other failure is uncertain.
func (v *VariablesHTTP) Post(ctx context.Context, variableID string, data map[string]any, recordDate string) (string, error) {
	enc, err := json.Marshal(data)
	if err != nil {
		return "", &PostError{Status: 0, Body: err.Error()}
	}
	body, _ := json.Marshal(map[string]string{"variableId": variableID, "data": string(enc), "recordDate": recordDate})
	code, b, err := v.do(ctx, http.MethodPost, "/values", body)
	if err != nil {
		return "", err
	}
	if code == http.StatusCreated || code == http.StatusOK {
		var out struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(b, &out) == nil && out.ID != "" {
			return out.ID, nil
		}
		return "", &statusError{"variables post without an id", code}
	}
	snippet := strings.TrimSpace(string(b))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	if code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests {
		return "", &PostError{Status: code, Body: snippet}
	}
	return "", &statusError{"variables post", code}
}

type wireValue struct {
	ID         string          `json:"id"`
	VariableID string          `json:"variableId"`
	Data       json.RawMessage `json:"data"`
	RecordDate string          `json:"recordDate"`
	RecordDat2 string          `json:"record_date"`
	CreatedAt  string          `json:"createdAt"`
}

func decodeValues(b []byte) ([]Value, error) {
	var ws []wireValue
	if err := json.Unmarshal(b, &ws); err != nil {
		return nil, err
	}
	out := make([]Value, 0, len(ws))
	for _, w := range ws {
		val := Value{ID: w.ID, VariableID: w.VariableID, RecordDate: w.RecordDate}
		if val.RecordDate == "" {
			val.RecordDate = w.RecordDat2
		}
		val.CreatedAt, _ = time.Parse(time.RFC3339Nano, w.CreatedAt)
		// data arrives as a JSON string holding the serialized object, or (per
		// the json type spec) as the parsed object itself. Accept both.
		var s string
		if json.Unmarshal(w.Data, &s) == nil {
			val.Raw = s
		} else {
			val.Raw = string(w.Data)
		}
		if strings.HasPrefix(strings.TrimSpace(val.Raw), "{") {
			dec := json.NewDecoder(strings.NewReader(val.Raw))
			var m map[string]any
			if dec.Decode(&m) == nil {
				val.Data = m
			}
		}
		out = append(out, val)
	}
	return out, nil
}

func (v *VariablesHTTP) ByDate(ctx context.Context, date string) ([]Value, error) {
	code, b, err := v.do(ctx, http.MethodGet, "/values?date="+url.QueryEscape(date)+"&includeJson=true", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, &statusError{"variables read " + date, code}
	}
	return decodeValues(b)
}

func (v *VariablesHTTP) All(ctx context.Context) ([]Value, error) {
	code, b, err := v.do(ctx, http.MethodGet, "/values?includeJson=true", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, &statusError{"variables read all", code}
	}
	return decodeValues(b)
}

// VarIDs are the resolved variable ids.
type VarIDs struct {
	Food, Body       string
	PushUps, PullUps string
	// Record variables (spec 18.4); "" = not set up.
	BP, Symptom string
}

// errConfig marks a definitive startup misconfiguration (not a network blip).
var errConfig = errors.New("fuel misconfigured")

// ResolveVars maps the configured names to ids: food and body must exist, be
// unique and be json. Push ups / Pull ups are optional numeric habits.
func ResolveVars(ctx context.Context, v Variables, food, body string) (VarIDs, error) {
	list, err := v.ListVariables(ctx)
	if err != nil {
		return VarIDs{}, err
	}
	return resolveFromList(list, food, body)
}

// resolveFromList is ResolveVars on a variable list that was read already.
func resolveFromList(list []VarInfo, food, body string) (VarIDs, error) {
	var err error
	find := func(name string, wantJSON bool, required bool) (string, error) {
		var hits []VarInfo
		for _, x := range list {
			if x.Name == name {
				hits = append(hits, x)
			}
		}
		switch {
		case len(hits) == 0 && !required:
			return "", nil
		case len(hits) == 0:
			return "", fmt.Errorf("%w: variable %q not found", errConfig, name)
		case len(hits) > 1:
			return "", fmt.Errorf("%w: variable name %q is not unique", errConfig, name)
		case wantJSON && hits[0].Type != "json":
			return "", fmt.Errorf("%w: variable %q is type %q, want json", errConfig, name, hits[0].Type)
		}
		return hits[0].ID, nil
	}
	var ids VarIDs
	if ids.Food, err = find(food, true, true); err != nil {
		return ids, err
	}
	if ids.Body, err = find(body, true, true); err != nil {
		return ids, err
	}
	ids.PushUps, _ = find("Push ups", false, false)
	ids.PullUps, _ = find("Pull ups", false, false)
	return ids, nil
}
