package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// GetAlertsInput optionally narrows by state. Validation happens in the
// handler (not the schema) so the error can name the valid values.
type GetAlertsInput struct {
	State string `json:"state,omitempty" jsonschema:"optional filter: firing or pending; empty returns both"`
}

// AlertInfo is one active alert.
type AlertInfo struct {
	Name        string            `json:"name" jsonschema:"the alertname label"`
	State       string            `json:"state" jsonschema:"firing (condition held for its full duration) or pending (still waiting it out)"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" jsonschema:"rule annotations such as summary and description"`
	ActiveAt    string            `json:"activeAt,omitempty" jsonschema:"RFC3339 time the alert became active"`
	Value       string            `json:"value,omitempty" jsonschema:"the alert expression's value at last evaluation"`
}

// GetAlertsOutput is the structured result.
type GetAlertsOutput struct {
	Alerts []AlertInfo `json:"alerts"`
	Total  int         `json:"total" jsonschema:"number of alerts after the state filter"`
	Hint   string      `json:"hint,omitempty"`
}

func (t *toolset) getAlerts(ctx context.Context, _ *mcp.CallToolRequest, in GetAlertsInput) (*mcp.CallToolResult, GetAlertsOutput, error) {
	var zero GetAlertsOutput

	if in.State != "" && in.State != "firing" && in.State != "pending" {
		return nil, zero, fmt.Errorf("invalid state %q: use \"firing\", \"pending\", or omit for both", in.State)
	}

	alerts, err := t.prom.Alerts(ctx)
	if err != nil {
		return nil, zero, fmt.Errorf("fetching alerts failed: %v - check that Prometheus is reachable (PROMSCOPE_PROMETHEUS_URL)", err)
	}

	out := GetAlertsOutput{Alerts: []AlertInfo{}}
	for _, a := range alerts {
		if in.State != "" && a.State != in.State {
			continue
		}
		out.Alerts = append(out.Alerts, AlertInfo{
			Name:        a.Name,
			State:       a.State,
			Labels:      a.Labels,
			Annotations: a.Annotations,
			ActiveAt:    a.ActiveAt.Format(time.RFC3339),
			Value:       a.Value,
		})
	}
	out.Total = len(out.Alerts)

	if out.Total == 0 {
		// Zero alerts is ambiguous to an agent: "all healthy" and "no rules
		// configured" look identical. Say so.
		out.Hint = "no active alerts - either everything is healthy or no alert rules are configured (read prometheus://rules to check)"
	}
	return nil, out, nil
}
