package terraform

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs/configschema"
	"github.com/hashicorp/terraform/plans"
	"github.com/hashicorp/terraform/providers"
	"github.com/hashicorp/terraform/states"
	"github.com/zclconf/go-cty/cty"
)

func TestContext2Plan_removedDuringRefresh(t *testing.T) {
	// The resource was added to state but actually failed to create and was
	// left tainted. This should be removed during plan and result in a Create
	// action.
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_object" "a" {
}
`,
	})

	p := simpleMockProvider()
	p.ReadResourceFn = func(req providers.ReadResourceRequest) (resp providers.ReadResourceResponse) {
		resp.NewState = cty.NullVal(req.PriorState.Type())
		return resp
	}

	addr := mustResourceInstanceAddr("test_object.a")
	state := states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(addr, &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{"test_string":"foo"}`),
			Status:    states.ObjectTainted,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
	})

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.Err())
	}

	for _, c := range plan.Changes.Resources {
		if c.Action != plans.Create {
			t.Fatalf("expected Create action for missing %s, got %s", c.Addr, c.Action)
		}
	}
}

func TestContext2Plan_noChangeDataSourceSensitiveNestedSet(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
variable "bar" {
  sensitive = true
  default   = "baz"
}

data "test_data_source" "foo" {
  foo {
    bar = var.bar
  }
}
`,
	})

	p := new(MockProvider)
	p.GetProviderSchemaResponse = getProviderSchemaResponseFromProviderSchema(&ProviderSchema{
		DataSources: map[string]*configschema.Block{
			"test_data_source": {
				Attributes: map[string]*configschema.Attribute{
					"id": {
						Type:     cty.String,
						Computed: true,
					},
				},
				BlockTypes: map[string]*configschema.NestedBlock{
					"foo": {
						Block: configschema.Block{
							Attributes: map[string]*configschema.Attribute{
								"bar": {Type: cty.String, Optional: true},
							},
						},
						Nesting: configschema.NestingSet,
					},
				},
			},
		},
	})

	p.ReadDataSourceResponse = &providers.ReadDataSourceResponse{
		State: cty.ObjectVal(map[string]cty.Value{
			"id":  cty.StringVal("data_id"),
			"foo": cty.SetVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"bar": cty.StringVal("baz")})}),
		}),
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("data.test_data_source.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"data_id", "foo":[{"bar":"baz"}]}`),
			AttrSensitivePaths: []cty.PathValueMarks{
				{
					Path:  cty.GetAttrPath("foo"),
					Marks: cty.NewValueMarks("sensitive"),
				},
			},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: state,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	for _, res := range plan.Changes.Resources {
		if res.Action != plans.NoOp {
			t.Fatalf("expected NoOp, got: %q %s", res.Addr, res.Action)
		}
	}
}

func TestContext2Plan_orphanDataInstance(t *testing.T) {
	// ensure the planned replacement of the data source is evaluated properly
	m := testModuleInline(t, map[string]string{
		"main.tf": `
data "test_object" "a" {
  for_each = { new = "ok" }
}

output "out" {
  value = [ for k, _ in data.test_object.a: k ]
}
`,
	})

	p := simpleMockProvider()
	p.ReadDataSourceFn = func(req providers.ReadDataSourceRequest) (resp providers.ReadDataSourceResponse) {
		resp.State = req.Config
		return resp
	}

	state := states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(mustResourceInstanceAddr(`data.test_object.a["old"]`), &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{"test_string":"foo"}`),
			Status:    states.ObjectReady,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
	})

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.Err())
	}

	change, err := plan.Changes.Outputs[0].Decode()
	if err != nil {
		t.Fatal(err)
	}

	expected := cty.TupleVal([]cty.Value{cty.StringVal("new")})

	if change.After.Equals(expected).False() {
		t.Fatalf("expected %#v, got %#v\n", expected, change.After)
	}
}

func TestContext2Plan_basicConfigurationAliases(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
provider "test" {
  alias = "z"
  test_string = "config"
}

module "mod" {
  source = "./mod"
  providers = {
    test.x = test.z
  }
}
`,

		"mod/main.tf": `
terraform {
  required_providers {
    test = {
      source = "registry.terraform.io/hashicorp/test"
      configuration_aliases = [ test.x ]
	}
  }
}

resource "test_object" "a" {
  provider = test.x
}

`,
	})

	p := simpleMockProvider()

	// The resource within the module should be using the provider configured
	// from the root module. We should never see an empty configuration.
	p.ConfigureProviderFn = func(req providers.ConfigureProviderRequest) (resp providers.ConfigureProviderResponse) {
		if req.Config.GetAttr("test_string").IsNull() {
			resp.Diagnostics = resp.Diagnostics.Append(errors.New("missing test_string value"))
		}
		return resp
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.Err())
	}
}

func TestContext2Plan_dataReferencesResourceInModules(t *testing.T) {
	p := testProvider("test")
	p.ReadDataSourceFn = func(req providers.ReadDataSourceRequest) (resp providers.ReadDataSourceResponse) {
		cfg := req.Config.AsValueMap()
		cfg["id"] = cty.StringVal("d")
		resp.State = cty.ObjectVal(cfg)
		return resp
	}

	m := testModuleInline(t, map[string]string{
		"main.tf": `
locals {
  things = {
    old = "first"
    new = "second"
  }
}

module "mod" {
  source = "./mod"
  for_each = local.things
}
`,

		"./mod/main.tf": `
resource "test_resource" "a" {
}

data "test_data_source" "d" {
  depends_on = [test_resource.a]
}

resource "test_resource" "b" {
  value = data.test_data_source.d.id
}
`})

	oldDataAddr := mustResourceInstanceAddr(`module.mod["old"].data.test_data_source.d`)

	state := states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(
			mustResourceInstanceAddr(`module.mod["old"].test_resource.a`),
			&states.ResourceInstanceObjectSrc{
				AttrsJSON: []byte(`{"id":"a"}`),
				Status:    states.ObjectReady,
			}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
		)
		s.SetResourceInstanceCurrent(
			mustResourceInstanceAddr(`module.mod["old"].test_resource.b`),
			&states.ResourceInstanceObjectSrc{
				AttrsJSON: []byte(`{"id":"b","value":"d"}`),
				Status:    states.ObjectReady,
			}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
		)
		s.SetResourceInstanceCurrent(
			oldDataAddr,
			&states.ResourceInstanceObjectSrc{
				AttrsJSON: []byte(`{"id":"d"}`),
				Status:    states.ObjectReady,
			}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
		)
	})

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: state,
	})

	plan, diags := ctx.Plan()
	assertNoErrors(t, diags)

	oldMod := oldDataAddr.Module

	for _, c := range plan.Changes.Resources {
		// there should be no changes from the old module instance
		if c.Addr.Module.Equal(oldMod) && c.Action != plans.NoOp {
			t.Errorf("unexpected change %s for %s\n", c.Action, c.Addr)
		}
	}
}

func TestContext2Plan_destroySkipRefresh(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_object" "a" {
}
`,
	})

	p := simpleMockProvider()

	addr := mustResourceInstanceAddr("test_object.a")
	state := states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(addr, &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{"test_string":"foo"}`),
			Status:    states.ObjectReady,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
	})

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		PlanMode:    plans.DestroyMode,
		SkipRefresh: true,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.Err())
	}

	if plan.State == nil {
		t.Fatal("missing plan state")
	}

	for _, c := range plan.Changes.Resources {
		if c.Action != plans.Delete {
			t.Errorf("unexpected %s change for %s", c.Action, c.Addr)
		}
	}
}

func TestContext2Plan_unmarkingSensitiveAttributeForOutput(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_resource" "foo" {
}

output "result" {
  value = nonsensitive(test_resource.foo.sensitive_attr)
}
`,
	})

	p := new(MockProvider)
	p.GetProviderSchemaResponse = getProviderSchemaResponseFromProviderSchema(&ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_resource": {
				Attributes: map[string]*configschema.Attribute{
					"id": {
						Type:     cty.String,
						Computed: true,
					},
					"sensitive_attr": {
						Type:      cty.String,
						Computed:  true,
						Sensitive: true,
					},
				},
			},
		},
	})

	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{
			PlannedState: cty.UnknownVal(cty.Object(map[string]cty.Type{
				"id":             cty.String,
				"sensitive_attr": cty.String,
			})),
		}
	}

	state := states.NewState()

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: state,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	for _, res := range plan.Changes.Resources {
		if res.Action != plans.Create {
			t.Fatalf("expected create, got: %q %s", res.Addr, res.Action)
		}
	}
}

func TestContext2Plan_destroyNoProviderConfig(t *testing.T) {
	// providers do not need to be configured during a destroy plan
	p := simpleMockProvider()
	p.ValidateProviderConfigFn = func(req providers.ValidateProviderConfigRequest) (resp providers.ValidateProviderConfigResponse) {
		v := req.Config.GetAttr("test_string")
		if v.IsNull() || !v.IsKnown() || v.AsString() != "ok" {
			resp.Diagnostics = resp.Diagnostics.Append(errors.New("invalid provider configuration"))
		}
		return resp
	}

	m := testModuleInline(t, map[string]string{
		"main.tf": `
locals {
  value = "ok"
}

provider "test" {
  test_string = local.value
}
`,
	})

	addr := mustResourceInstanceAddr("test_object.a")
	state := states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(addr, &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{"test_string":"foo"}`),
			Status:    states.ObjectReady,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
	})

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		PlanMode: plans.DestroyMode,
	})

	_, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.Err())
	}
}

func TestContext2Plan_refreshOnlyMode(t *testing.T) {
	addr := mustResourceInstanceAddr("test_object.a")

	p := simpleMockProvider()

	// The configuration, the prior state, and the refresh result intentionally
	// have different values for "test_string" so we can observe that the
	// refresh took effect but the configuration change wasn't considered.
	m := testModuleInline(t, map[string]string{
		"main.tf": `
			resource "test_object" "a" {
				test_string = "after"
			}
		`,
	})
	state := states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(addr, &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{"test_string":"before"}`),
			Status:    states.ObjectReady,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
	})
	p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		newVal, err := cty.Transform(req.PriorState, func(path cty.Path, v cty.Value) (cty.Value, error) {
			if len(path) == 1 && path[0] == (cty.GetAttrStep{Name: "test_string"}) {
				return cty.StringVal("current"), nil
			}
			return v, nil
		})
		if err != nil {
			// shouldn't get here
			t.Fatalf("ReadResourceFn transform failed")
			return providers.ReadResourceResponse{}
		}
		return providers.ReadResourceResponse{
			NewState: newVal,
		}
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		PlanMode: plans.RefreshOnlyMode,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("unexpected errors\n%s", diags.Err().Error())
	}

	if got, want := len(plan.Changes.Resources), 0; got != want {
		t.Fatalf("plan contains resource changes; want none\n%s", spew.Sdump(plan.Changes.Resources))
	}

	state = plan.State
	instState := state.ResourceInstance(addr)
	if instState == nil {
		t.Fatalf("%s has no state at all after plan", addr)
	}
	if instState.Current == nil {
		t.Fatalf("%s has no current object after plan", addr)
	}
	if got, want := instState.Current.AttrsJSON, `"current"`; !bytes.Contains(got, []byte(want)) {
		t.Fatalf("%s has wrong prior state after plan\ngot:\n%s\n\nwant substring: %s", addr, got, want)
	}
}

func TestContext2Plan_invalidSensitiveModuleOutput(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"child/main.tf": `
output "out" {
  value = sensitive("xyz")
}`,
		"main.tf": `
module "child" {
  source = "./child"
}

output "root" {
  value = module.child.out
}`,
	})

	ctx := testContext2(t, &ContextOpts{
		Config: m,
	})

	_, diags := ctx.Plan()
	if !diags.HasErrors() {
		t.Fatal("succeeded; want errors")
	}
	if got, want := diags.Err().Error(), "Output refers to sensitive values"; !strings.Contains(got, want) {
		t.Fatalf("wrong error:\ngot:  %s\nwant: message containing %q", got, want)
	}
}

func TestContext2Plan_planDataSourceSensitiveNested(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "test_instance" "bar" {
}

data "test_data_source" "foo" {
  foo {
    bar = test_instance.bar.sensitive
  }
}
`,
	})

	p := new(MockProvider)
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) (resp providers.PlanResourceChangeResponse) {
		resp.PlannedState = cty.ObjectVal(map[string]cty.Value{
			"sensitive": cty.UnknownVal(cty.String),
		})
		return resp
	}
	p.GetProviderSchemaResponse = getProviderSchemaResponseFromProviderSchema(&ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_instance": {
				Attributes: map[string]*configschema.Attribute{
					"sensitive": {
						Type:      cty.String,
						Computed:  true,
						Sensitive: true,
					},
				},
			},
		},
		DataSources: map[string]*configschema.Block{
			"test_data_source": {
				Attributes: map[string]*configschema.Attribute{
					"id": {
						Type:     cty.String,
						Computed: true,
					},
				},
				BlockTypes: map[string]*configschema.NestedBlock{
					"foo": {
						Block: configschema.Block{
							Attributes: map[string]*configschema.Attribute{
								"bar": {Type: cty.String, Optional: true},
							},
						},
						Nesting: configschema.NestingSet,
					},
				},
			},
		},
	})

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("data.test_data_source.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"string":"data_id", "foo":[{"bar":"old"}]}`),
			AttrSensitivePaths: []cty.PathValueMarks{
				{
					Path:  cty.GetAttrPath("foo"),
					Marks: cty.NewValueMarks("sensitive"),
				},
			},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"sensitive":"old"}`),
			AttrSensitivePaths: []cty.PathValueMarks{
				{
					Path:  cty.GetAttrPath("sensitive"),
					Marks: cty.NewValueMarks("sensitive"),
				},
			},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: state,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	for _, res := range plan.Changes.Resources {
		switch res.Addr.String() {
		case "test_instance.bar":
			if res.Action != plans.Update {
				t.Fatalf("unexpected %s change for %s", res.Action, res.Addr)
			}
		case "data.test_data_source.foo":
			if res.Action != plans.Read {
				t.Fatalf("unexpected %s change for %s", res.Action, res.Addr)
			}
		default:
			t.Fatalf("unexpected %s change for %s", res.Action, res.Addr)
		}
	}
}

func TestContext2Plan_forceReplace(t *testing.T) {
	addrA := mustResourceInstanceAddr("test_object.a")
	addrB := mustResourceInstanceAddr("test_object.b")
	m := testModuleInline(t, map[string]string{
		"main.tf": `
			resource "test_object" "a" {
			}
			resource "test_object" "b" {
			}
		`,
	})

	state := states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(addrA, &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{}`),
			Status:    states.ObjectReady,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
		s.SetResourceInstanceCurrent(addrB, &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{}`),
			Status:    states.ObjectReady,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
	})

	p := simpleMockProvider()
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		ForceReplace: []addrs.AbsResourceInstance{
			addrA,
		},
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("unexpected errors\n%s", diags.Err().Error())
	}

	t.Run(addrA.String(), func(t *testing.T) {
		instPlan := plan.Changes.ResourceInstance(addrA)
		if instPlan == nil {
			t.Fatalf("no plan for %s at all", addrA)
		}

		if got, want := instPlan.Action, plans.DeleteThenCreate; got != want {
			t.Errorf("wrong planned action\ngot:  %s\nwant: %s", got, want)
		}
		if got, want := instPlan.ActionReason, plans.ResourceInstanceReplaceByRequest; got != want {
			t.Errorf("wrong action reason\ngot:  %s\nwant: %s", got, want)
		}
	})
	t.Run(addrB.String(), func(t *testing.T) {
		instPlan := plan.Changes.ResourceInstance(addrB)
		if instPlan == nil {
			t.Fatalf("no plan for %s at all", addrB)
		}

		if got, want := instPlan.Action, plans.NoOp; got != want {
			t.Errorf("wrong planned action\ngot:  %s\nwant: %s", got, want)
		}
		if got, want := instPlan.ActionReason, plans.ResourceInstanceChangeNoReason; got != want {
			t.Errorf("wrong action reason\ngot:  %s\nwant: %s", got, want)
		}
	})
}

func TestContext2Plan_forceReplaceIncompleteAddr(t *testing.T) {
	addr0 := mustResourceInstanceAddr("test_object.a[0]")
	addr1 := mustResourceInstanceAddr("test_object.a[1]")
	addrBare := mustResourceInstanceAddr("test_object.a")
	m := testModuleInline(t, map[string]string{
		"main.tf": `
			resource "test_object" "a" {
				count = 2
			}
		`,
	})

	state := states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(addr0, &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{}`),
			Status:    states.ObjectReady,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
		s.SetResourceInstanceCurrent(addr1, &states.ResourceInstanceObjectSrc{
			AttrsJSON: []byte(`{}`),
			Status:    states.ObjectReady,
		}, mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`))
	})

	p := simpleMockProvider()
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		ForceReplace: []addrs.AbsResourceInstance{
			addrBare,
		},
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("unexpected errors\n%s", diags.Err().Error())
	}
	diagsErr := diags.ErrWithWarnings()
	if diagsErr == nil {
		t.Fatalf("no warnings were returned")
	}
	if got, want := diagsErr.Error(), "Incompletely-matched force-replace resource instance"; !strings.Contains(got, want) {
		t.Errorf("missing expected warning\ngot:\n%s\n\nwant substring: %s", got, want)
	}

	t.Run(addr0.String(), func(t *testing.T) {
		instPlan := plan.Changes.ResourceInstance(addr0)
		if instPlan == nil {
			t.Fatalf("no plan for %s at all", addr0)
		}

		if got, want := instPlan.Action, plans.NoOp; got != want {
			t.Errorf("wrong planned action\ngot:  %s\nwant: %s", got, want)
		}
		if got, want := instPlan.ActionReason, plans.ResourceInstanceChangeNoReason; got != want {
			t.Errorf("wrong action reason\ngot:  %s\nwant: %s", got, want)
		}
	})
	t.Run(addr1.String(), func(t *testing.T) {
		instPlan := plan.Changes.ResourceInstance(addr1)
		if instPlan == nil {
			t.Fatalf("no plan for %s at all", addr1)
		}

		if got, want := instPlan.Action, plans.NoOp; got != want {
			t.Errorf("wrong planned action\ngot:  %s\nwant: %s", got, want)
		}
		if got, want := instPlan.ActionReason, plans.ResourceInstanceChangeNoReason; got != want {
			t.Errorf("wrong action reason\ngot:  %s\nwant: %s", got, want)
		}
	})
}
