package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crypticseeds/promscope/internal/promclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startSession wires a real mcp.Server (with our tools registered over the
// given fake) to a real mcp.Client across the SDK's in-memory transport.
// Everything the wire would exercise - schema validation, isError
// conversion, structured content - runs for real; only the network and
// Prometheus are absent.
func startSession(t *testing.T, fake *fakeClient) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "promscope-test", Version: "test"}, nil)
	Register(server, fake, DefaultLimits())

	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestIntegrationToolCatalog(t *testing.T) {
	session := startSession(t, &fakeClient{})

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	want := map[string]bool{"list_metrics": false, "get_alerts": false, "query_metrics": false}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q - the surface is frozen at three", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must carry readOnlyHint", tool.Name)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s must declare input AND output schemas", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q missing from catalog", name)
		}
	}
}

func TestIntegrationStructuredContent(t *testing.T) {
	session := startSession(t, &fakeClient{
		names: []string{"up", "vllm:num_requests_running"},
		meta:  map[string]promclient.Meta{"up": {Type: "gauge", Help: "scrape ok"}},
	})

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_metrics",
		Arguments: map[string]any{"filter": "up"},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}

	// StructuredContent must round-trip into our declared output shape.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out ListMetricsOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structured content does not match ListMetricsOutput: %v", err)
	}
	if out.Total != 1 || out.Metrics[0].Name != "up" || out.Metrics[0].Type != "gauge" {
		t.Errorf("unexpected structured output: %+v", out)
	}
}

func TestIntegrationSchemaValidationRejectsBadInput(t *testing.T) {
	session := startSession(t, &fakeClient{names: []string{"up"}})

	// filter must be a string; the SDK validates against the inferred
	// schema before our handler runs. Our fake would happily answer, so a
	// rejection proves validation happened in the protocol layer.
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_metrics",
		Arguments: map[string]any{"filter": 42},
	})
	if err != nil {
		// Some SDK versions surface validation as a protocol error instead
		// of a tool error; either is a rejection, which is what we require.
		return
	}
	if !res.IsError {
		t.Fatalf("bad input must be rejected, got success: %+v", res.StructuredContent)
	}
}

func TestIntegrationToolErrorConversion(t *testing.T) {
	session := startSession(t, &fakeClient{namesErr: fmt.Errorf("connection refused")})

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_metrics",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("upstream failure must be a tool error, not a protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected isError=true")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "PROMSCOPE_PROMETHEUS_URL") {
		t.Errorf("agent-facing error must name the fix, got %+v", res.Content[0])
	}
}

func TestIntegrationRulesResource(t *testing.T) {
	session := startSession(t, &fakeClient{
		ruleGroups: []promclient.RuleGroup{{
			Name: "demo",
			Rules: []promclient.Rule{{
				Kind: "alerting", Name: "AlwaysFiring", Query: "vector(1)",
				For: 5 * time.Minute, Labels: map[string]string{"severity": "info"},
			}},
		}},
	})
	ctx := context.Background()

	list, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	if len(list.Resources) != 1 || list.Resources[0].URI != "prometheus://rules" {
		t.Fatalf("want exactly prometheus://rules in the catalog, got %+v", list.Resources)
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "prometheus://rules"})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	if res.Contents[0].MIMEType != "application/json" {
		t.Errorf("MIMEType = %q, want application/json", res.Contents[0].MIMEType)
	}

	var doc struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
				For  string `json:"for"`
			} `json:"rules"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &doc); err != nil {
		t.Fatalf("resource body is not the documented JSON shape: %v", err)
	}
	r := doc.Groups[0].Rules[0]
	if r.Kind != "alerting" || r.Name != "AlwaysFiring" || r.For != "5m0s" {
		t.Errorf("unexpected rule in document: %+v", r)
	}
}

func TestIntegrationRulesResourceUpstreamError(t *testing.T) {
	session := startSession(t, &fakeClient{rulesErr: fmt.Errorf("connection refused")})

	_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "prometheus://rules"})
	if err == nil {
		t.Fatal("want error when Prometheus is unreachable")
	}
	if !strings.Contains(err.Error(), "PROMSCOPE_PROMETHEUS_URL") {
		t.Errorf("resource error must name the fix, got: %v", err)
	}
}

func TestIntegrationUnknownTool(t *testing.T) {
	session := startSession(t, &fakeClient{})

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "drop_database"})
	if err == nil {
		t.Fatal("unknown tool must fail")
	}
}
