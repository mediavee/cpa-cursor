package cursor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// unaryServer fakes api2: HTTP/1.1 only, like the CLI's unary transport.
type unaryServer struct {
	srv    *httptest.Server
	calls  atomic.Int32
	last   *http.Request
	body   []byte
	status int
	reply  string
}

func newUnaryServer(t *testing.T) (*unaryServer, *Client) {
	t.Helper()
	u := &unaryServer{status: http.StatusOK, reply: `{}`}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		u.last = r
		u.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = io.WriteString(w, u.reply)
	}))
	t.Cleanup(u.srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(u.srv.Certificate())
	cfg := DefaultConfig()
	cfg.APIURL, cfg.SiteURL = u.srv.URL, u.srv.URL
	c, err := NewClient(cfg, WithTLSConfig(&tls.Config{RootCAs: pool}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return u, c
}

func TestUnaryRequestShape(t *testing.T) {
	u, c := newUnaryServer(t)
	u.reply = `{"planInfo":{"planName":"Ultra","price":"$200/mo","billingCycleEnd":"1794489320000","includedUsagePeriod":"INCLUDED_USAGE_PERIOD_MONTHLY"}}`
	got, err := c.GetPlanInfo(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	r := u.last
	if r.Method != http.MethodPost || r.URL.Path != "/aiserver.v1.DashboardService/GetPlanInfo" {
		t.Errorf("request = %s %s", r.Method, r.URL.Path)
	}
	if r.ProtoMajor != 1 {
		t.Errorf("unary call over HTTP/%d, the CLI uses HTTP/1.1", r.ProtoMajor)
	}
	for k, v := range map[string]string{
		"Authorization": "Bearer tok", "Content-Type": "application/json", "Connect-Protocol-Version": "1",
		"X-Ghost-Mode": "true", "X-Cursor-Client-Type": "cli", "User-Agent": "connect-es/1.6.1",
		"X-Cursor-Client-Version": DefaultClientVersion,
	} {
		if r.Header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, r.Header.Get(k), v)
		}
	}
	if string(u.body) != `{}` {
		t.Errorf("body = %s", u.body)
	}
	if got.PlanInfo.PlanName != "Ultra" || got.PlanInfo.BillingCycleEnd != 1794489320000 || got.PlanInfo.BillingCycleEnd.Time().IsZero() {
		t.Errorf("plan = %+v", got)
	}
}

func TestAvailableModelsSendsTheCLIsRequest(t *testing.T) {
	u, c := newUnaryServer(t)
	u.reply = `{"models":[{"name":"grok-9","serverModelName":"grok-9","supportsImages":true,"contextTokenLimit":256000,
	  "parameterDefinitions":[
	    {"id":"effort","name":"Effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"high","displayName":"High"}]}}},
	    {"id":"fast","name":"Fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true","increasesModelCost":true}]}}}],
	  "variants":[{"parameterValues":[{"id":"effort","value":"low"},{"id":"fast","value":"false"}],"variantStringRepresentation":"grok-9[effort=low,fast=false]","legacySlug":"grok-9-low","isDefaultNonMaxConfig":true}]}]}`
	got, err := c.AvailableModels(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if u.last.URL.Path != "/aiserver.v1.AiService/AvailableModels" {
		t.Errorf("path = %s", u.last.URL.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(u.body, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent["useModelParameters"] != true || sent["doNotUseMarkdown"] != true {
		t.Errorf("body = %s, want the CLI's {useModelParameters, doNotUseMarkdown, additionalModelNames}", u.body)
	}
	if _, ok := sent["additionalModelNames"].([]any); !ok {
		t.Errorf("additionalModelNames = %v, want a list", sent["additionalModelNames"])
	}
	m := got.Models[0]
	if m.Name != "grok-9" || !m.SupportsImages || m.ContextTokenLimit != 256000 {
		t.Errorf("model = %+v", m)
	}
	if len(m.ParameterDefinitions) != 2 || len(m.ParameterDefinitions[0].Values()) != 2 || !m.ParameterDefinitions[1].Values()[1].IncreasesModelCost {
		t.Errorf("parameters = %+v", m.ParameterDefinitions)
	}
	v := m.Variants[0]
	if v.VariantStringRepresentation != "grok-9[effort=low,fast=false]" || len(v.ParameterValues) != 2 || !v.IsDefaultNonMaxConfig {
		t.Errorf("variant = %+v", v)
	}
}

func TestGetUsableModelsDecodesTheCLIsMessage(t *testing.T) {
	u, c := newUnaryServer(t)
	u.reply = `{"models":[{"modelId":"composer-9","displayModelId":"composer-9","displayName":"Composer 9","aliases":["c9"],"maxMode":false,"someFutureField":1}]}`
	models, err := c.GetUsableModels(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if u.last.URL.Path != "/agent.v1.AgentService/GetUsableModels" {
		t.Errorf("path = %s", u.last.URL.Path)
	}
	if len(models) != 1 || models[0].GetModelId() != "composer-9" || models[0].GetAliases()[0] != "c9" {
		t.Errorf("models = %v", models)
	}
}

func TestUsageEndpointsDecodeInt64Strings(t *testing.T) {
	u, c := newUnaryServer(t)
	u.reply = `{"billingCycleStart":"1791897320000","billingCycleEnd":"1794489320000",
	  "planUsage":{"totalSpend":100,"limit":40000,"autoPercentUsed":94.5,"apiPercentUsed":4.36,"totalPercentUsed":91.2},
	  "displayMessage":"msg","autoBucketModels":["default","composer-2.5"]}`
	usage, err := c.GetCurrentPeriodUsage(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if usage.BillingCycleEnd.Time().UnixMilli() != 1794489320000 || usage.PlanUsage.AutoPercentUsed != 94.5 || usage.PlanUsage.Limit != 40000 ||
		len(usage.AutoBucketModels) != 2 {
		t.Errorf("usage = %+v", usage)
	}

	u.reply = `{"usageLimitPolicyStatus":{"isInSlowPool":true,"errorTitle":"Limit","slownessMs":1500,"stage":"LIMIT_HIT_STAGE_SLOW_POOL","resetAtMs":"1794489320000","limitType":"user"}}`
	st, err := c.GetUsageLimitStatusAndActiveGrants(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	p := st.Policy
	if !p.IsInSlowPool || p.SlownessMs != 1500 || p.ResetAtMs != 1794489320000 || p.Stage != "LIMIT_HIT_STAGE_SLOW_POOL" || p.ErrorTitle != "Limit" {
		t.Errorf("policy = %+v", p)
	}
}

func TestUnaryErrorsAreClassified(t *testing.T) {
	u, c := newUnaryServer(t)
	u.status = http.StatusUnauthorized
	u.reply = `{"code":"unauthenticated","message":"token expired"}`
	_, err := c.GetPlanInfo(context.Background(), "tok")
	var ce *Error
	if !errors.As(err, &ce) || ce.Class != ClassLogin || ce.HTTPStatus != 401 {
		t.Fatalf("err = %v", err)
	}
}

func TestServerConfigIsCachedAndRedirectsRuns(t *testing.T) {
	up := newFakeUpstream(t)
	other := newFakeUpstream(t) // the agent host the server config announces
	api, c := newUnaryServer(t)
	_ = api
	api.reply = `{"agentUrlConfig":{"agentUrl":"` + other.srv.URL + `","agentnUrl":"https://ignored.example.com"}}`
	cfg := up.config()
	cfg.APIURL = api.srv.URL
	pool := x509.NewCertPool()
	pool.AddCert(api.srv.Certificate())
	pool.AddCert(other.srv.Certificate())
	c2, err := NewClient(cfg, WithTLSConfig(&tls.Config{RootCAs: pool}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c2.CloseIdleConnections)
	_ = c

	for range 3 {
		sc, err := c2.GetServerConfig(context.Background(), "tok")
		if err != nil {
			t.Fatal(err)
		}
		if sc.AgentURL(true) != other.srv.URL || sc.AgentURL(false) != "https://ignored.example.com" {
			t.Fatalf("server config = %+v", sc)
		}
	}
	if n := api.calls.Load(); n != 1 {
		t.Errorf("GetServerConfig hit the network %d times, want 1 (cached)", n)
	}

	s, err := c2.Run(context.Background(), "tok", RunParams{Request: runRequest("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other.accept() // the run went to the announced host, not to Config.AgentURL
	select {
	case <-up.streams:
		t.Fatal("run went to the configured agent URL instead of the announced one")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServerConfigRejectsAnUnusableAgentURL(t *testing.T) {
	u, c := newUnaryServer(t)
	u.reply = `{"agentUrlConfig":{"agentUrl":"http://plain.example.com"}}`
	if _, err := c.GetServerConfig(context.Background(), "tok"); err == nil {
		t.Fatal("a non-https agent URL was accepted")
	}
	if c.agentURL() != DefaultAgentURL {
		t.Error("an unusable agent URL replaced the default")
	}
}
