// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package do

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/spf13/cobra"

	cmdBackend "github.com/pulumi/pulumi/pkg/v3/cmd/pulumi/backend"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	pkgWorkspace "github.com/pulumi/pulumi/pkg/v3/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func doResourceSpec(withList bool) schema.PackageSpec {
	res := schema.ResourceSpec{
		ObjectTypeSpec: schema.ObjectTypeSpec{
			Description: "A test resource.",
			Properties: map[string]schema.PropertySpec{
				"name":    {TypeSpec: schema.TypeSpec{Type: "string"}},
				"size":    {TypeSpec: schema.TypeSpec{Type: "integer"}},
				"enabled": {TypeSpec: schema.TypeSpec{Type: "boolean"}},
				"extra":   {TypeSpec: schema.TypeSpec{Type: "string"}},
			},
		},
		InputProperties: map[string]schema.PropertySpec{
			"name": {
				TypeSpec:    schema.TypeSpec{Type: "string"},
				Description: "The resource name.",
			},
			"size": {
				TypeSpec: schema.TypeSpec{Type: "integer"},
			},
			"enabled": {
				TypeSpec: schema.TypeSpec{Type: "boolean"},
			},
		},
		RequiredInputs: []string{"name"},
	}
	if withList {
		res.ListInputs = &schema.ObjectTypeSpec{
			Properties: map[string]schema.PropertySpec{
				"prefix": {TypeSpec: schema.TypeSpec{Type: "string"}},
			},
		}
	}
	return schema.PackageSpec{
		Name: "azure",
		Resources: map[string]schema.ResourceSpec{
			"azure:index:myResource": res,
		},
	}
}

func newDoResourceCommand(
	t *testing.T, provider *testProvider,
) (*cobraCommand, *bytes.Buffer) {
	t.Helper()

	mlm := &cmdBackend.MockLoginManager{}
	mws := &pkgWorkspace.MockContext{}
	loader := func(ctx context.Context, sink diag.Sink, wd, source string) (io.Closer, plugin.Provider, error) {
		assert.Equal(t, "azure", source)
		return closer(t), provider, nil
	}

	var stdout bytes.Buffer
	cmd := NewDoCmd(mlm, mws, loader)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	return (*cobraCommand)(cmd), &stdout
}

type cobraCommand = cobra.Command

func TestDoCmdResourceHelpListsOperations(t *testing.T) {
	t.Parallel()

	cmd, stdout := newDoResourceCommand(t, &testProvider{spec: doResourceSpec(true)})
	cmd.SetArgs([]string{"azure", "myResource", "--help"})
	err := cmd.Execute()
	require.NoError(t, err)

	output := stdout.String()
	assert.Contains(t, output, "Operate on the myResource resource.")
	assert.Contains(t, output, "Inputs:")
	assert.Contains(t, output, "List Inputs:")
	assert.Contains(t, output, "create")
	assert.Contains(t, output, "read")
	assert.Contains(t, output, "patch")
	assert.Contains(t, output, "delete")
	assert.Contains(t, output, "list")
}

func TestDoCmdResourceHelpOmitsListWithoutListInputs(t *testing.T) {
	t.Parallel()

	cmd, stdout := newDoResourceCommand(t, &testProvider{spec: doResourceSpec(false)})
	cmd.SetArgs([]string{"azure", "myResource", "--help"})
	err := cmd.Execute()
	require.NoError(t, err)

	assert.NotContains(t, stdout.String(), "list")
}

func TestDoCmdResourceCreate(t *testing.T) {
	t.Parallel()

	var calls []string
	cmd, stdout := newDoResourceCommand(t, &testProvider{
		spec: doResourceSpec(false),
		MockProvider: plugin.MockProvider{
			CheckF: func(ctx context.Context, req plugin.CheckRequest) (plugin.CheckResponse, error) {
				calls = append(calls, "check")
				assert.Equal(t, tokens.Type("azure:index:myResource"), req.Type)
				assert.Equal(t, "example", req.News["name"].StringValue())
				assert.Equal(t, 2.0, req.News["size"].NumberValue())
				return plugin.CheckResponse{Properties: req.News}, nil
			},
			CreateF: func(ctx context.Context, req plugin.CreateRequest) (plugin.CreateResponse, error) {
				calls = append(calls, "create")
				assert.Equal(t, "example", req.Properties["name"].StringValue())
				return plugin.CreateResponse{
					ID: "res-1",
					Properties: resource.PropertyMap{
						"name":  resource.NewProperty("example"),
						"size":  resource.NewProperty(2.0),
						"extra": resource.NewProperty("hidden"),
					},
				}, nil
			},
		},
	})

	inputFile := writeHCLFile(t, "inputs.pcl", `
name = "example"
size = 2
`)
	cmd.SetArgs([]string{"azure", "myResource", "create", "--input-file", inputFile})
	err := cmd.Execute()
	require.NoError(t, err)

	assert.Equal(t, []string{"check", "create"}, calls)
	assert.JSONEq(t, `{
  "id": "res-1",
  "properties": {
    "name": "example",
    "size": 2,
    "extra": "hidden"
  }
}`, stdout.String())
}

func TestDoCmdResourceReadDeletePatch(t *testing.T) {
	t.Parallel()

	t.Run("read", func(t *testing.T) {
		t.Parallel()
		cmd, stdout := newDoResourceCommand(t, &testProvider{
			spec: doResourceSpec(false),
			MockProvider: plugin.MockProvider{
				ReadF: func(ctx context.Context, req plugin.ReadRequest) (plugin.ReadResponse, error) {
					assert.Equal(t, resource.ID("res-1"), req.ID)
					return plugin.ReadResponse{
						ReadResult: plugin.ReadResult{
							ID: "res-1",
							Outputs: resource.PropertyMap{
								"name": resource.NewProperty("read"),
								"size": resource.NewProperty(3.0),
							},
						},
					}, nil
				},
			},
		})
		cmd.SetArgs([]string{"azure", "myResource", "read", "res-1"})
		err := cmd.Execute()
		require.NoError(t, err)
		assert.JSONEq(t, `{"id":"res-1","properties":{"name":"read","size":3}}`, stdout.String())
	})

	t.Run("delete", func(t *testing.T) {
		t.Parallel()
		var deleted bool
		cmd, stdout := newDoResourceCommand(t, &testProvider{
			spec: doResourceSpec(false),
			MockProvider: plugin.MockProvider{
				DeleteF: func(ctx context.Context, req plugin.DeleteRequest) (plugin.DeleteResponse, error) {
					deleted = true
					assert.Equal(t, resource.ID("res-1"), req.ID)
					assert.Empty(t, req.Inputs)
					assert.Empty(t, req.Outputs)
					return plugin.DeleteResponse{}, nil
				},
			},
		})
		cmd.SetArgs([]string{"azure", "myResource", "delete", "res-1"})
		err := cmd.Execute()
		require.NoError(t, err)
		assert.True(t, deleted)
		assert.Empty(t, stdout.String())
	})

	t.Run("patch", func(t *testing.T) {
		t.Parallel()
		var calls []string
		cmd, stdout := newDoResourceCommand(t, &testProvider{
			spec: doResourceSpec(false),
			MockProvider: plugin.MockProvider{
				ReadF: func(ctx context.Context, req plugin.ReadRequest) (plugin.ReadResponse, error) {
					calls = append(calls, "read")
					return plugin.ReadResponse{
						ReadResult: plugin.ReadResult{
							ID: "res-1",
							Inputs: resource.PropertyMap{
								"name":    resource.NewProperty("old"),
								"size":    resource.NewProperty(1.0),
								"enabled": resource.NewProperty(false),
							},
							Outputs: resource.PropertyMap{
								"name":    resource.NewProperty("old"),
								"size":    resource.NewProperty(1.0),
								"enabled": resource.NewProperty(false),
							},
						},
					}, nil
				},
				CheckF: func(ctx context.Context, req plugin.CheckRequest) (plugin.CheckResponse, error) {
					calls = append(calls, "check")
					assert.Equal(t, "old", req.Olds["name"].StringValue())
					assert.Equal(t, "new", req.News["name"].StringValue())
					assert.Equal(t, 1.0, req.News["size"].NumberValue())
					assert.Equal(t, true, req.News["enabled"].BoolValue())
					return plugin.CheckResponse{Properties: req.News}, nil
				},
				UpdateF: func(ctx context.Context, req plugin.UpdateRequest) (plugin.UpdateResponse, error) {
					calls = append(calls, "update")
					assert.Equal(t, "new", req.NewInputs["name"].StringValue())
					assert.Equal(t, 1.0, req.NewInputs["size"].NumberValue())
					assert.Equal(t, true, req.NewInputs["enabled"].BoolValue())
					return plugin.UpdateResponse{
						Properties: resource.PropertyMap{
							"name":    resource.NewProperty("new"),
							"size":    resource.NewProperty(1.0),
							"enabled": resource.NewProperty(true),
						},
					}, nil
				},
			},
		})

		inputFile := writeHCLFile(t, "patch.pcl", `
name = "new"
enabled = true
`)
		cmd.SetArgs([]string{"azure", "myResource", "patch", "res-1", "--input-file", inputFile})
		err := cmd.Execute()
		require.NoError(t, err)
		assert.Equal(t, []string{"read", "check", "update"}, calls)
		assert.JSONEq(t, `{"id":"res-1","properties":{"name":"new","size":1,"enabled":true}}`, stdout.String())
	})
}

func TestDoCmdResourceList(t *testing.T) {
	t.Parallel()

	t.Run("single page by default", func(t *testing.T) {
		t.Parallel()
		var calls []plugin.ListRequest
		cmd, stdout := newDoResourceCommand(t, &testProvider{
			spec: doResourceSpec(true),
			MockProvider: plugin.MockProvider{
				ListF: func(ctx context.Context, req plugin.ListRequest) (plugin.ListResponse, error) {
					calls = append(calls, req)
					assert.Equal(t, "prod", req.Query["prefix"].StringValue())
					return plugin.ListResponse{
						Results:           []plugin.ListResult{{ID: "1", Name: "one"}},
						ContinuationToken: "next",
					}, nil
				},
			},
		})
		inputFile := writeHCLFile(t, "list.pcl", `prefix = "prod"`)
		cmd.SetArgs([]string{"azure", "myResource", "list", "--input-file", inputFile})
		err := cmd.Execute()
		require.NoError(t, err)
		require.Len(t, calls, 1)
		assert.Empty(t, calls[0].ContinuationToken)
		assert.JSONEq(t, `[{"id":"1","name":"one"}]`, stdout.String())
	})

	t.Run("count", func(t *testing.T) {
		t.Parallel()
		var calls []plugin.ListRequest
		cmd, stdout := newDoResourceCommand(t, &testProvider{
			spec: doResourceSpec(true),
			MockProvider: plugin.MockProvider{
				ListF: func(ctx context.Context, req plugin.ListRequest) (plugin.ListResponse, error) {
					calls = append(calls, req)
					if req.ContinuationToken == "" {
						return plugin.ListResponse{
							Results:           []plugin.ListResult{{ID: "1", Name: "one"}},
							ContinuationToken: "next",
						}, nil
					}
					return plugin.ListResponse{
						Results: []plugin.ListResult{{ID: "2", Name: "two"}, {ID: "3", Name: "three"}},
					}, nil
				},
			},
		})
		cmd.SetArgs([]string{"azure", "myResource", "list", "--count", "2"})
		err := cmd.Execute()
		require.NoError(t, err)
		require.Len(t, calls, 2)
		assert.Equal(t, int64(2), calls[0].Limit)
		assert.Equal(t, int64(1), calls[1].Limit)
		assert.JSONEq(t, `[{"id":"1","name":"one"},{"id":"2","name":"two"}]`, stdout.String())
	})

	t.Run("all", func(t *testing.T) {
		t.Parallel()
		var calls []plugin.ListRequest
		cmd, stdout := newDoResourceCommand(t, &testProvider{
			spec: doResourceSpec(true),
			MockProvider: plugin.MockProvider{
				ListF: func(ctx context.Context, req plugin.ListRequest) (plugin.ListResponse, error) {
					calls = append(calls, req)
					if req.ContinuationToken == "" {
						return plugin.ListResponse{
							Results:           []plugin.ListResult{{ID: "1", Name: "one"}},
							ContinuationToken: "next",
						}, nil
					}
					return plugin.ListResponse{
						Results: []plugin.ListResult{{ID: "2", Name: "two"}},
					}, nil
				},
			},
		})
		cmd.SetArgs([]string{"azure", "myResource", "list", "--all"})
		err := cmd.Execute()
		require.NoError(t, err)
		require.Len(t, calls, 2)
		assert.Equal(t, "next", calls[1].ContinuationToken)
		assert.JSONEq(t, `[{"id":"1","name":"one"},{"id":"2","name":"two"}]`, stdout.String())
	})

	t.Run("mutually exclusive flags", func(t *testing.T) {
		t.Parallel()
		cmd, _ := newDoResourceCommand(t, &testProvider{spec: doResourceSpec(true)})
		cmd.SetArgs([]string{"azure", "myResource", "list", "--all", "--count", "1"})
		err := cmd.Execute()
		require.ErrorContains(t, err, "--all and --count are mutually exclusive")
	})
}
