package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rulesURI is promscope's one MCP resource (SPEC section 3). Resources are
// the read-only document side of MCP: where a tool answers a question, a
// resource is a thing the client can fetch (and, on stateful servers,
// subscribe to - subscriptions need a session and are out of scope here).
const rulesURI = "prometheus://rules"

// ruleJSON and ruleGroupJSON are the wire shapes of the resource document -
// agent-friendly (for as "5m0s", omitempty noise control), decoupled from
// promclient types like every other output in this package.
type ruleJSON struct {
	Kind        string            `json:"kind"` // alerting | recording
	Name        string            `json:"name"`
	Query       string            `json:"query"`
	For         string            `json:"for,omitempty"` // alerting: condition must hold this long
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Health      string            `json:"health,omitempty"`
	LastError   string            `json:"lastError,omitempty"`
}

type ruleGroupJSON struct {
	Name  string     `json:"name"`
	File  string     `json:"file,omitempty"`
	Rules []ruleJSON `json:"rules"`
}

type rulesDocJSON struct {
	Groups []ruleGroupJSON `json:"groups"`
	Note   string          `json:"note,omitempty"`
}

func (t *toolset) readRules(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	groups, err := t.prom.Rules(ctx)
	if err != nil {
		// Resources have no isError channel; a protocol error with an
		// actionable message is the only honest option.
		return nil, fmt.Errorf("reading rules failed: %v - check that Prometheus is reachable (PROMSCOPE_PROMETHEUS_URL)", err)
	}

	doc := rulesDocJSON{Groups: make([]ruleGroupJSON, len(groups))}
	for i, g := range groups {
		gj := ruleGroupJSON{Name: g.Name, File: g.File, Rules: make([]ruleJSON, len(g.Rules))}
		for j, r := range g.Rules {
			rj := ruleJSON{
				Kind:        r.Kind,
				Name:        r.Name,
				Query:       r.Query,
				Labels:      r.Labels,
				Annotations: r.Annotations,
				Health:      r.Health,
				LastError:   r.LastError,
			}
			if r.For > 0 {
				rj.For = r.For.String()
			}
			gj.Rules[j] = rj
		}
		doc.Groups[i] = gj
	}
	if len(doc.Groups) == 0 {
		doc.Note = "no alerting or recording rules are configured on this Prometheus - get_alerts will always be empty"
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding rules: %v", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      rulesURI,
			MIMEType: "application/json",
			Text:     string(body),
		}},
	}, nil
}
