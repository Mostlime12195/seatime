package promptcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DecisionsURL is OpenRouter's decisions API.
const DecisionsURL = "https://openrouter.ai/api/alpha/decisions"

// Judge answers the questions about the state, with the model and API
// address in t. The server uses Client; tests use a fake.
type Judge interface {
	Judge(ctx context.Context, t Target, state string, qs map[string]Question) (Answer, error)
}

// Target is one model to ask and where (URL "" is DecisionsURL). The
// server asks its targets in order until one answers.
type Target struct {
	Model string `json:"model"`
	URL   string `json:"url,omitempty"`
}

// Answer is the model's reply: a probability per question, the model's
// full name with its version, and what the call cost in dollars.
type Answer struct {
	P     map[string]float64
	Model string
	Cost  float64
}

// Client calls the decisions API. Key is the OpenRouter key from the
// server's configuration; it is sent only in the Authorization header and
// is never part of an error or a log line. The call is bounded by ctx
// (the server's prompt_check_timeout_s setting).
type Client struct {
	Key  string
	HTTP *http.Client
}

type decisionsReq struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]Question `json:"questions"`
	Provider  map[string]any      `json:"provider"`
}

type decisionsResp struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Noul *float64 `json:"noul"`
	} `json:"answers"`
	Usage struct {
		Cost float64 `json:"cost"`
	} `json:"usage"`
}

// ErrNoKey: the switch is on but no key is configured.
var ErrNoKey = errors.New("promptcheck: no OpenRouter key configured")

// Judge implements Judge. Providers must not keep the data (zdr).
func (c *Client) Judge(ctx context.Context, t Target, state string, qs map[string]Question) (Answer, error) {
	if c.Key == "" {
		return Answer{}, ErrNoKey
	}
	url, model := t.URL, t.Model
	if url == "" {
		url = DecisionsURL
	}
	if model == "" {
		model = Model
	}
	body, err := json.Marshal(decisionsReq{Model: model, State: state, Questions: qs, Provider: map[string]any{"zdr": true}})
	if err != nil {
		return Answer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute} // a backstop: ctx is the real bound
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) { // drop the URL and anything else the transport echoes
			return Answer{}, fmt.Errorf("promptcheck: decisions call failed: %w", ue.Unwrap())
		}
		return Answer{}, errors.New("promptcheck: decisions call failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Answer{}, fmt.Errorf("promptcheck: reading the answer: %w", err)
	}
	if resp.StatusCode != 200 {
		// the body is the provider's error; it can quote the request, so
		// only the status goes into the error
		return Answer{}, fmt.Errorf("promptcheck: decisions API answered %d", resp.StatusCode)
	}
	var r decisionsResp
	if err := json.Unmarshal(b, &r); err != nil {
		return Answer{}, errors.New("promptcheck: the decisions API answer is not JSON")
	}
	a := Answer{P: map[string]float64{}, Model: r.Model, Cost: r.Usage.Cost}
	if a.Model == "" {
		a.Model = model
	}
	for k, v := range r.Answers {
		if v.Noul != nil {
			a.P[k] = *v.Noul
		}
	}
	return a, nil
}
