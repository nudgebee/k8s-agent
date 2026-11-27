package mutate

import (
	"context"
	"testing"

	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// With split identities, PrometheusRules in the agent's namespace go through
// the runner's own client and rules anywhere else through the change client.
func TestRulesClient_ByNamespace(t *testing.T) {
	writer := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	own := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(writer)
	m.SetIdentityDynamics(own, nil)
	m.SetNamespace("nudgebee-agent")

	if _, err := m.CreateOrReplaceAlertRule(context.Background(), LegacyAlertRuleParams{Alert: "A", Expr: "up == 0"}); err != nil {
		t.Fatalf("own-namespace rule: %v", err)
	}
	if n := len(own.Actions()); n == 0 {
		t.Fatal("own-namespace rule did not use the runner's own client")
	}
	if n := countWrites(writer); n != 0 {
		t.Fatalf("own-namespace rule wrote through the change client (%d writes)", n)
	}

	if err := m.DeletePrometheusRule(context.Background(), "monitoring", "team-rules"); err != nil {
		t.Fatalf("other-namespace delete: %v", err)
	}
	if n := countWrites(writer); n == 0 {
		t.Fatal("rule in another namespace did not use the change client")
	}
}

// Without identity clients every rule uses the one client (legacy mode).
func TestRulesClient_LegacySingleClient(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")
	if _, err := m.CreateOrReplaceAlertRule(context.Background(), LegacyAlertRuleParams{Alert: "A", Expr: "up == 0"}); err != nil {
		t.Fatal(err)
	}
	if countWrites(dyn) == 0 {
		t.Fatal("legacy mode did not use the single client")
	}
}

func countWrites(c *dynamicfake.FakeDynamicClient) int {
	n := 0
	for _, a := range c.Actions() {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete":
			n++
		}
	}
	return n
}
