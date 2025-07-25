package dockerfile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/proxy"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/continuity/fs/fstest"
	"github.com/containerd/platforms"
	intoto "github.com/in-toto/in-toto-golang/in_toto"
	provenanceCommon "github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/common"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend/dockerui"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/identity"
	provenancetypes "github.com/moby/buildkit/solver/llbsolver/provenance/types"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/testutil"
	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/moby/buildkit/util/testutil/workers"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"github.com/tonistiigi/fsutil"
)

var provenanceTests = integration.TestFuncs(
	testProvenanceAttestation,
	testGitProvenanceAttestation,
	testMultiPlatformProvenance,
	testClientFrontendProvenance,
	testClientLLBProvenance,
	testSecretSSHProvenance,
	testOCILayoutProvenance,
	testNilProvenance,
	testDuplicatePlatformProvenance,
	testDockerIgnoreMissingProvenance,
	testCommandSourceMapping,
	testFrontendDeduplicateSources,
	testDuplicateLayersProvenance,
	testProvenanceExportLocal,
	testProvenanceExportLocalForceSplit,
	testProvenanceExportLocalMultiPlatform,
	testProvenanceExportLocalMultiPlatformNoSplit,
)

func init() {
	allTests = append(allTests, provenanceTests...)
}

func testProvenanceAttestation(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	f := getFrontend(t, sb)

	dockerfile := []byte(integration.UnixOrWindows(
		`
FROM busybox:latest
RUN echo "ok" > /foo
`,
		`
FROM nanoserver
USER ContainerAdministrator
RUN echo ok> /foo
`,
	))

	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	for _, slsaVersion := range []string{"", "v1", "v0.2"} {
		for _, mode := range []string{"", "min", "max"} {
			var tname []string
			if slsaVersion != "" {
				tname = append(tname, slsaVersion)
			}
			if mode != "" {
				tname = append(tname, mode)
			}
			t.Run(strings.Join(tname, "-"), func(t *testing.T) {
				var target string
				if target == "" {
					target = registry + "/buildkit/testwithprovenance:none"
				} else {
					target = registry + "/buildkit/testwithprovenance:" + mode
				}

				var provArgs []string
				if slsaVersion != "" {
					provArgs = append(provArgs, "version="+slsaVersion)
				}
				if mode != "" {
					provArgs = append(provArgs, "mode="+mode)
				}
				_, err = f.Solve(sb.Context(), c, client.SolveOpt{
					LocalMounts: map[string]fsutil.FS{
						dockerui.DefaultLocalNameDockerfile: dir,
						dockerui.DefaultLocalNameContext:    dir,
					},
					FrontendAttrs: map[string]string{
						"attest:provenance": strings.Join(provArgs, ","),
						"build-arg:FOO":     "bar",
						"label:lbl":         "abc",
						"vcs:source":        "https://user:pass@example.invalid/repo.git",
						"vcs:revision":      "123456",
						"filename":          "Dockerfile",
						dockerui.DefaultLocalNameContext + ":foo": "https://foo:bar@example.invalid/foo.html",
					},
					Exports: []client.ExportEntry{
						{
							Type: client.ExporterImage,
							Attrs: map[string]string{
								"name": target,
								"push": "true",
							},
						},
					},
				}, nil)
				require.NoError(t, err)

				desc, provider, err := contentutil.ProviderFromRef(target)
				require.NoError(t, err)
				imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
				require.NoError(t, err)
				require.Equal(t, 2, len(imgs.Images))

				img := imgs.Find(platforms.Format(platforms.Normalize(platforms.DefaultSpec())))
				require.NotNil(t, img)
				outFile := integration.UnixOrWindows("foo", "Files/foo")
				expectedFileData := integration.UnixOrWindows([]byte("ok\n"), []byte("ok\r\n"))
				require.Equal(t, expectedFileData, img.Layers[1][outFile].Data)

				att := imgs.Find("unknown/unknown")
				require.NotNil(t, att)
				require.Equal(t, string(img.Desc.Digest), att.Desc.Annotations["vnd.docker.reference.digest"])
				require.Equal(t, "attestation-manifest", att.Desc.Annotations["vnd.docker.reference.type"])
				var attest intoto.Statement
				require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
				require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)

				if slsaVersion == "v1" {
					require.Equal(t, "https://slsa.dev/provenance/v1", attest.PredicateType) // intentionally not const
				} else {
					require.Equal(t, "https://slsa.dev/provenance/v0.2", attest.PredicateType) // intentionally not const
				}

				_, isClient := f.(*clientFrontend)
				_, isGateway := f.(*gatewayFrontend)

				if slsaVersion == "v1" {
					type stmtT struct {
						Predicate provenancetypes.ProvenancePredicateSLSA1 `json:"predicate"`
					}
					var stmt stmtT
					require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
					pred := stmt.Predicate

					require.Equal(t, "https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md", pred.BuildDefinition.BuildType)
					require.Equal(t, "", pred.RunDetails.Builder.ID)

					require.Equal(t, "", pred.BuildDefinition.ExternalParameters.ConfigSource.URI)

					args := pred.BuildDefinition.ExternalParameters.Request.Args
					if isClient {
						require.Equal(t, "", pred.BuildDefinition.ExternalParameters.Request.Frontend)
						require.Equal(t, 0, len(args), "%v", args)
						require.False(t, pred.RunDetails.Metadata.Completeness.Request)
						require.Equal(t, "", pred.BuildDefinition.ExternalParameters.ConfigSource.Path)
					} else if isGateway {
						require.Equal(t, "gateway.v0", pred.BuildDefinition.ExternalParameters.Request.Frontend)

						if mode == "max" || mode == "" {
							require.Equal(t, 4, len(args), "%v", args)
							require.True(t, pred.RunDetails.Metadata.Completeness.Request)

							require.Equal(t, "bar", args["build-arg:FOO"])
							require.Equal(t, "abc", args["label:lbl"])
							require.Contains(t, args["source"], "buildkit_test/")
						} else {
							require.False(t, pred.RunDetails.Metadata.Completeness.Request)
							require.Equal(t, 2, len(args), "%v", args)
							require.Contains(t, args["source"], "buildkit_test/")
						}
						require.Equal(t, "https://xxxxx:xxxxx@example.invalid/foo.html", args["context:foo"])
					} else {
						require.Equal(t, "dockerfile.v0", pred.BuildDefinition.ExternalParameters.Request.Frontend)

						if mode == "max" || mode == "" {
							require.Equal(t, 3, len(args))
							require.True(t, pred.RunDetails.Metadata.Completeness.Request)

							require.Equal(t, "bar", args["build-arg:FOO"])
							require.Equal(t, "abc", args["label:lbl"])
						} else {
							require.False(t, pred.RunDetails.Metadata.Completeness.Request)
							require.Equal(t, 1, len(args), "%v", args)
						}
						require.Equal(t, "https://xxxxx:xxxxx@example.invalid/foo.html", args["context:foo"])
					}

					expectedBaseImage := integration.UnixOrWindows("busybox", "nanoserver")
					escapedPlatform := url.PathEscape(platforms.Format(platforms.Normalize(platforms.DefaultSpec())))
					expectedBase := fmt.Sprintf("pkg:docker/%s@latest?platform=%s", expectedBaseImage, escapedPlatform)
					if isGateway {
						require.Equal(t, 2, len(pred.BuildDefinition.ResolvedDependencies), "%+v", pred.BuildDefinition.ResolvedDependencies)
						require.Contains(t, pred.BuildDefinition.ResolvedDependencies[0].URI, "docker/buildkit_test")
						require.Equal(t, expectedBase, pred.BuildDefinition.ResolvedDependencies[1].URI)
						require.NotEmpty(t, pred.BuildDefinition.ResolvedDependencies[1].Digest["sha256"])
					} else {
						require.Equal(t, 1, len(pred.BuildDefinition.ResolvedDependencies), "%+v", pred.BuildDefinition.ResolvedDependencies)
						require.Equal(t, expectedBase, pred.BuildDefinition.ResolvedDependencies[0].URI)
						require.NotEmpty(t, pred.BuildDefinition.ResolvedDependencies[0].Digest["sha256"])
					}

					if !isClient {
						require.Equal(t, "Dockerfile", pred.BuildDefinition.ExternalParameters.ConfigSource.Path)
						require.Equal(t, "https://xxxxx:xxxxx@example.invalid/repo.git", pred.RunDetails.Metadata.BuildKitMetadata.VCS["source"])
						require.Equal(t, "123456", pred.RunDetails.Metadata.BuildKitMetadata.VCS["revision"])
					}

					require.NotEmpty(t, pred.RunDetails.Metadata.InvocationID)

					require.Equal(t, 2, len(pred.BuildDefinition.ExternalParameters.Request.Locals), "%+v", pred.BuildDefinition.ExternalParameters.Request.Locals)
					require.Equal(t, "context", pred.BuildDefinition.ExternalParameters.Request.Locals[0].Name)
					require.Equal(t, "dockerfile", pred.BuildDefinition.ExternalParameters.Request.Locals[1].Name)

					require.NotNil(t, pred.RunDetails.Metadata.FinishedOn)
					require.Less(t, time.Since(*pred.RunDetails.Metadata.FinishedOn), 5*time.Minute)
					require.NotNil(t, pred.RunDetails.Metadata.StartedOn)
					require.Less(t, time.Since(*pred.RunDetails.Metadata.StartedOn), 5*time.Minute)
					require.True(t, pred.RunDetails.Metadata.StartedOn.Before(*pred.RunDetails.Metadata.FinishedOn))

					require.Equal(t, platforms.Format(platforms.Normalize(platforms.DefaultSpec())), pred.BuildDefinition.InternalParameters.BuilderPlatform)

					require.False(t, pred.RunDetails.Metadata.Completeness.ResolvedDependencies)
					require.False(t, pred.RunDetails.Metadata.Reproducible)
					require.False(t, pred.RunDetails.Metadata.Hermetic)

					if mode == "max" || mode == "" {
						require.Equal(t, 2, len(pred.RunDetails.Metadata.BuildKitMetadata.Layers))
						require.NotNil(t, pred.RunDetails.Metadata.BuildKitMetadata.Source)
						require.Equal(t, "Dockerfile", pred.RunDetails.Metadata.BuildKitMetadata.Source.Infos[0].Filename)
						require.Equal(t, dockerfile, pred.RunDetails.Metadata.BuildKitMetadata.Source.Infos[0].Data)
						require.NotNil(t, pred.BuildDefinition.InternalParameters.BuildConfig)
						require.Equal(t, 3, len(pred.BuildDefinition.InternalParameters.BuildConfig.Definition))
					} else {
						require.Equal(t, 0, len(pred.RunDetails.Metadata.BuildKitMetadata.Layers))
						require.Nil(t, pred.RunDetails.Metadata.BuildKitMetadata.Source)
						require.Nil(t, pred.BuildDefinition.InternalParameters.BuildConfig)
					}
				} else {
					type stmtT struct {
						Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
					}
					var stmt stmtT
					require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
					pred := stmt.Predicate

					require.Equal(t, "https://mobyproject.org/buildkit@v1", pred.BuildType)
					require.Equal(t, "", pred.Builder.ID)

					require.Equal(t, "", pred.Invocation.ConfigSource.URI)

					args := pred.Invocation.Parameters.Args
					if isClient {
						require.Equal(t, "", pred.Invocation.Parameters.Frontend)
						require.Equal(t, 0, len(args), "%v", args)
						require.False(t, pred.Metadata.Completeness.Parameters)
						require.Equal(t, "", pred.Invocation.ConfigSource.EntryPoint)
					} else if isGateway {
						require.Equal(t, "gateway.v0", pred.Invocation.Parameters.Frontend)

						if mode == "max" || mode == "" {
							require.Equal(t, 4, len(args), "%v", args)
							require.True(t, pred.Metadata.Completeness.Parameters)

							require.Equal(t, "bar", args["build-arg:FOO"])
							require.Equal(t, "abc", args["label:lbl"])
							require.Contains(t, args["source"], "buildkit_test/")
						} else {
							require.False(t, pred.Metadata.Completeness.Parameters)
							require.Equal(t, 2, len(args), "%v", args)
							require.Contains(t, args["source"], "buildkit_test/")
						}
						require.Equal(t, "https://xxxxx:xxxxx@example.invalid/foo.html", args["context:foo"])
					} else {
						require.Equal(t, "dockerfile.v0", pred.Invocation.Parameters.Frontend)

						if mode == "max" || mode == "" {
							require.Equal(t, 3, len(args))
							require.True(t, pred.Metadata.Completeness.Parameters)

							require.Equal(t, "bar", args["build-arg:FOO"])
							require.Equal(t, "abc", args["label:lbl"])
						} else {
							require.False(t, pred.Metadata.Completeness.Parameters)
							require.Equal(t, 1, len(args), "%v", args)
						}
						require.Equal(t, "https://xxxxx:xxxxx@example.invalid/foo.html", args["context:foo"])
					}

					expectedBaseImage := integration.UnixOrWindows("busybox", "nanoserver")
					escapedPlatform := url.PathEscape(platforms.Format(platforms.Normalize(platforms.DefaultSpec())))
					expectedBase := fmt.Sprintf("pkg:docker/%s@latest?platform=%s", expectedBaseImage, escapedPlatform)
					if isGateway {
						require.Equal(t, 2, len(pred.Materials), "%+v", pred.Materials)
						require.Contains(t, pred.Materials[0].URI, "docker/buildkit_test")
						require.Equal(t, expectedBase, pred.Materials[1].URI)
						require.NotEmpty(t, pred.Materials[1].Digest["sha256"])
					} else {
						require.Equal(t, 1, len(pred.Materials), "%+v", pred.Materials)
						require.Equal(t, expectedBase, pred.Materials[0].URI)
						require.NotEmpty(t, pred.Materials[0].Digest["sha256"])
					}

					if !isClient {
						require.Equal(t, "Dockerfile", pred.Invocation.ConfigSource.EntryPoint)
						require.Equal(t, "https://xxxxx:xxxxx@example.invalid/repo.git", pred.Metadata.BuildKitMetadata.VCS["source"])
						require.Equal(t, "123456", pred.Metadata.BuildKitMetadata.VCS["revision"])
					}

					require.NotEmpty(t, pred.Metadata.BuildInvocationID)

					require.Equal(t, 2, len(pred.Invocation.Parameters.Locals), "%+v", pred.Invocation.Parameters.Locals)
					require.Equal(t, "context", pred.Invocation.Parameters.Locals[0].Name)
					require.Equal(t, "dockerfile", pred.Invocation.Parameters.Locals[1].Name)

					require.NotNil(t, pred.Metadata.BuildFinishedOn)
					require.Less(t, time.Since(*pred.Metadata.BuildFinishedOn), 5*time.Minute)
					require.NotNil(t, pred.Metadata.BuildStartedOn)
					require.Less(t, time.Since(*pred.Metadata.BuildStartedOn), 5*time.Minute)
					require.True(t, pred.Metadata.BuildStartedOn.Before(*pred.Metadata.BuildFinishedOn))

					require.True(t, pred.Metadata.Completeness.Environment)
					require.Equal(t, platforms.Format(platforms.Normalize(platforms.DefaultSpec())), pred.Invocation.Environment.Platform)

					require.False(t, pred.Metadata.Completeness.Materials)
					require.False(t, pred.Metadata.Reproducible)
					require.False(t, pred.Metadata.Hermetic)

					if mode == "max" || mode == "" {
						require.Equal(t, 2, len(pred.Metadata.BuildKitMetadata.Layers))
						require.NotNil(t, pred.Metadata.BuildKitMetadata.Source)
						require.Equal(t, "Dockerfile", pred.Metadata.BuildKitMetadata.Source.Infos[0].Filename)
						require.Equal(t, dockerfile, pred.Metadata.BuildKitMetadata.Source.Infos[0].Data)
						require.NotNil(t, pred.BuildConfig)

						require.Equal(t, 3, len(pred.BuildConfig.Definition))
					} else {
						require.Equal(t, 0, len(pred.Metadata.BuildKitMetadata.Layers))
						require.Nil(t, pred.Metadata.BuildKitMetadata.Source)
						require.Nil(t, pred.BuildConfig)
					}
				}
			})
		}
	}
}

func testGitProvenanceAttestation(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	f := getFrontend(t, sb)

	for _, slsaVersion := range []string{"", "v1", "v0.2"} {
		t.Run(slsaVersion, func(t *testing.T) {
			var provArgs []string
			if slsaVersion != "" {
				provArgs = append(provArgs, "version="+slsaVersion)
			}

			dockerfile := []byte(`
FROM busybox:latest
RUN --network=none echo "git" > /foo
COPY myapp.Dockerfile /
`)
			dir := integration.Tmpdir(
				t,
				fstest.CreateFile("myapp.Dockerfile", dockerfile, 0600),
			)

			err = runShell(dir.Name,
				"git init",
				"git config --local user.email test",
				"git config --local user.name test",
				"git add myapp.Dockerfile",
				"git commit -m initial",
				"git branch v1",
				"git update-server-info",
			)
			require.NoError(t, err)

			cmd := exec.Command("git", "rev-parse", "v1")
			cmd.Dir = dir.Name
			expectedGitSHA, err := cmd.Output()
			require.NoError(t, err)

			server := httptest.NewServer(http.FileServer(http.Dir(filepath.Clean(dir.Name))))
			defer server.Close()

			target := registry + "/buildkit/testwithprovenance:git"

			// inject dummy credentials to test that they are masked
			expectedURL := strings.Replace(server.URL, "http://", "http://xxxxx:xxxxx@", 1)
			require.NotEqual(t, expectedURL, server.URL)
			server.URL = strings.Replace(server.URL, "http://", "http://user:pass@", 1)

			_, err = f.Solve(sb.Context(), c, client.SolveOpt{
				FrontendAttrs: map[string]string{
					"context":           server.URL + "/.git#v1",
					"attest:provenance": strings.Join(provArgs, ","),
					"filename":          "myapp.Dockerfile",
				},
				Exports: []client.ExportEntry{
					{
						Type: client.ExporterImage,
						Attrs: map[string]string{
							"name": target,
							"push": "true",
						},
					},
				},
			}, nil)
			require.NoError(t, err)

			desc, provider, err := contentutil.ProviderFromRef(target)
			require.NoError(t, err)
			imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
			require.NoError(t, err)
			require.Equal(t, 2, len(imgs.Images))

			img := imgs.Find(platforms.Format(platforms.Normalize(platforms.DefaultSpec())))
			require.NotNil(t, img)
			require.Equal(t, []byte("git\n"), img.Layers[1]["foo"].Data)

			att := imgs.Find("unknown/unknown")
			require.NotNil(t, att)
			require.Equal(t, string(img.Desc.Digest), att.Desc.Annotations["vnd.docker.reference.digest"])
			require.Equal(t, "attestation-manifest", att.Desc.Annotations["vnd.docker.reference.type"])
			var attest intoto.Statement
			require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
			require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)

			_, isClient := f.(*clientFrontend)
			_, isGateway := f.(*gatewayFrontend)

			if slsaVersion == "v1" {
				require.Equal(t, "https://slsa.dev/provenance/v1", attest.PredicateType) // intentionally not const

				type stmtT struct {
					Predicate provenancetypes.ProvenancePredicateSLSA1 `json:"predicate"`
				}
				var stmt stmtT
				require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
				pred := stmt.Predicate

				if isClient {
					require.Empty(t, pred.BuildDefinition.ExternalParameters.Request.Frontend)
					require.Equal(t, "", pred.BuildDefinition.ExternalParameters.ConfigSource.URI)
					require.Equal(t, "", pred.BuildDefinition.ExternalParameters.ConfigSource.Path)
				} else {
					require.NotEmpty(t, pred.BuildDefinition.ExternalParameters.Request.Frontend)
					require.Equal(t, expectedURL+"/.git#v1", pred.BuildDefinition.ExternalParameters.ConfigSource.URI)
					require.Equal(t, "myapp.Dockerfile", pred.BuildDefinition.ExternalParameters.ConfigSource.Path)
				}

				expBase := "pkg:docker/busybox@latest?platform=" + url.PathEscape(platforms.Format(platforms.Normalize(platforms.DefaultSpec())))
				if isGateway {
					require.Equal(t, 3, len(pred.BuildDefinition.ResolvedDependencies), "%+v", pred.BuildDefinition.ResolvedDependencies)

					require.Contains(t, pred.BuildDefinition.ResolvedDependencies[0].URI, "pkg:docker/buildkit_test/")
					require.NotEmpty(t, pred.BuildDefinition.ResolvedDependencies[0].Digest)

					require.Equal(t, expBase, pred.BuildDefinition.ResolvedDependencies[1].URI)
					require.NotEmpty(t, pred.BuildDefinition.ResolvedDependencies[1].Digest["sha256"])

					require.Equal(t, expectedURL+"/.git#v1", pred.BuildDefinition.ResolvedDependencies[2].URI)
					require.Equal(t, strings.TrimSpace(string(expectedGitSHA)), pred.BuildDefinition.ResolvedDependencies[2].Digest["sha1"])
				} else {
					require.Equal(t, 2, len(pred.BuildDefinition.ResolvedDependencies), "%+v", pred.BuildDefinition.ResolvedDependencies)

					require.Equal(t, expBase, pred.BuildDefinition.ResolvedDependencies[0].URI)
					require.NotEmpty(t, pred.BuildDefinition.ResolvedDependencies[0].Digest["sha256"])

					require.Equal(t, expectedURL+"/.git#v1", pred.BuildDefinition.ResolvedDependencies[1].URI)
					require.Equal(t, strings.TrimSpace(string(expectedGitSHA)), pred.BuildDefinition.ResolvedDependencies[1].Digest["sha1"])
				}

				require.Equal(t, 0, len(pred.BuildDefinition.ExternalParameters.Request.Locals))

				require.True(t, pred.RunDetails.Metadata.Completeness.ResolvedDependencies)
				require.True(t, pred.RunDetails.Metadata.Hermetic)

				if isClient {
					require.False(t, pred.RunDetails.Metadata.Completeness.Request)
				} else {
					require.True(t, pred.RunDetails.Metadata.Completeness.Request)
				}
				require.False(t, pred.RunDetails.Metadata.Reproducible)

				require.Equal(t, 0, len(pred.RunDetails.Metadata.BuildKitMetadata.VCS), "%+v", pred.RunDetails.Metadata.BuildKitMetadata.VCS)
			} else {
				require.Equal(t, "https://slsa.dev/provenance/v0.2", attest.PredicateType) // intentionally not const

				type stmtT struct {
					Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
				}
				var stmt stmtT
				require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
				pred := stmt.Predicate

				if isClient {
					require.Empty(t, pred.Invocation.Parameters.Frontend)
					require.Equal(t, "", pred.Invocation.ConfigSource.URI)
					require.Equal(t, "", pred.Invocation.ConfigSource.EntryPoint)
				} else {
					require.NotEmpty(t, pred.Invocation.Parameters.Frontend)
					require.Equal(t, expectedURL+"/.git#v1", pred.Invocation.ConfigSource.URI)
					require.Equal(t, "myapp.Dockerfile", pred.Invocation.ConfigSource.EntryPoint)
				}

				expBase := "pkg:docker/busybox@latest?platform=" + url.PathEscape(platforms.Format(platforms.Normalize(platforms.DefaultSpec())))
				if isGateway {
					require.Equal(t, 3, len(pred.Materials), "%+v", pred.Materials)

					require.Contains(t, pred.Materials[0].URI, "pkg:docker/buildkit_test/")
					require.NotEmpty(t, pred.Materials[0].Digest)

					require.Equal(t, expBase, pred.Materials[1].URI)
					require.NotEmpty(t, pred.Materials[1].Digest["sha256"])

					require.Equal(t, expectedURL+"/.git#v1", pred.Materials[2].URI)
					require.Equal(t, strings.TrimSpace(string(expectedGitSHA)), pred.Materials[2].Digest["sha1"])
				} else {
					require.Equal(t, 2, len(pred.Materials), "%+v", pred.Materials)

					require.Equal(t, expBase, pred.Materials[0].URI)
					require.NotEmpty(t, pred.Materials[0].Digest["sha256"])

					require.Equal(t, expectedURL+"/.git#v1", pred.Materials[1].URI)
					require.Equal(t, strings.TrimSpace(string(expectedGitSHA)), pred.Materials[1].Digest["sha1"])
				}

				require.Equal(t, 0, len(pred.Invocation.Parameters.Locals))

				require.True(t, pred.Metadata.Completeness.Materials)
				require.True(t, pred.Metadata.Completeness.Environment)
				require.True(t, pred.Metadata.Hermetic)

				if isClient {
					require.False(t, pred.Metadata.Completeness.Parameters)
				} else {
					require.True(t, pred.Metadata.Completeness.Parameters)
				}
				require.False(t, pred.Metadata.Reproducible)

				require.Equal(t, 0, len(pred.Metadata.BuildKitMetadata.VCS), "%+v", pred.Metadata.BuildKitMetadata.VCS)
			}
		})
	}
}

func testMultiPlatformProvenance(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureMultiPlatform, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	f := getFrontend(t, sb)

	dockerfile := []byte(`
FROM busybox:latest
ARG TARGETARCH
RUN echo "ok-$TARGETARCH" > /foo
`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	target := registry + "/buildkit/testmultiprovenance:latest"

	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
			"build-arg:FOO":     "bar",
			"label:lbl":         "abc",
			"platform":          "linux/amd64,linux/arm64",
		},
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 4, len(imgs.Images))

	_, isClient := f.(*clientFrontend)
	_, isGateway := f.(*gatewayFrontend)

	for _, p := range []string{"linux/amd64", "linux/arm64"} {
		img := imgs.Find(p)
		require.NotNil(t, img)
		if p == "linux/amd64" {
			require.Equal(t, []byte("ok-amd64\n"), img.Layers[1]["foo"].Data)
		} else {
			require.Equal(t, []byte("ok-arm64\n"), img.Layers[1]["foo"].Data)
		}

		att := imgs.FindAttestation(p)
		require.NotNil(t, att)
		require.Equal(t, "attestation-manifest", att.Desc.Annotations["vnd.docker.reference.type"])
		var attest intoto.Statement
		require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
		require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
		require.Equal(t, "https://slsa.dev/provenance/v0.2", attest.PredicateType) // intentionally not const

		type stmtT struct {
			Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
		}
		var stmt stmtT
		require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
		pred := stmt.Predicate

		require.Equal(t, "https://mobyproject.org/buildkit@v1", pred.BuildType)
		require.Equal(t, "", pred.Builder.ID)
		require.Equal(t, "", pred.Invocation.ConfigSource.URI)

		if isGateway {
			require.Equal(t, 2, len(pred.Materials), "%+v", pred.Materials)
			require.Contains(t, pred.Materials[0].URI, "buildkit_test")
			require.Contains(t, pred.Materials[1].URI, "pkg:docker/busybox@latest")
			require.Contains(t, pred.Materials[1].URI, url.PathEscape(p))
		} else {
			require.Equal(t, 1, len(pred.Materials), "%+v", pred.Materials)
			require.Contains(t, pred.Materials[0].URI, "pkg:docker/busybox@latest")
			require.Contains(t, pred.Materials[0].URI, url.PathEscape(p))
		}

		args := pred.Invocation.Parameters.Args
		if isClient {
			require.Equal(t, 0, len(args), "%+v", args)
		} else if isGateway {
			require.Equal(t, 3, len(args), "%+v", args)
			require.Equal(t, "bar", args["build-arg:FOO"])
			require.Equal(t, "abc", args["label:lbl"])
			require.Contains(t, args["source"], "buildkit_test/")
		} else {
			require.Equal(t, 2, len(args), "%+v", args)
			require.Equal(t, "bar", args["build-arg:FOO"])
			require.Equal(t, "abc", args["label:lbl"])
		}
	}
}

func testClientFrontendProvenance(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureProvenance)
	// Building with client frontend does not capture frontend provenance
	// because frontend runs in client, not in BuildKit.
	// This test builds Dockerfile inside a client frontend ensuring that
	// in that case frontend provenance is captured.
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target := registry + "/buildkit/clientprovenance:latest"

	f := getFrontend(t, sb)

	_, isClient := f.(*clientFrontend)
	if !isClient {
		t.Skip("not a client frontend")
	}

	dockerfile := []byte(`
	FROM alpine as x86target
	RUN echo "alpine" > /foo

	FROM busybox:latest AS armtarget
	RUN --network=none echo "bbox" > /foo
	`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		st := llb.HTTP("https://raw.githubusercontent.com/moby/moby/v20.10.21/README.md")
		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, err
		}
		// This does not show up in provenance
		res0, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		dt, err := res0.Ref.ReadFile(ctx, gateway.ReadRequest{
			Filename: "README.md",
		})
		if err != nil {
			return nil, err
		}

		res1, err := c.Solve(ctx, gateway.SolveRequest{
			Frontend: "dockerfile.v0",
			FrontendOpt: map[string]string{
				"build-arg:FOO": string(dt[:3]),
				"target":        "armtarget",
			},
		})
		if err != nil {
			return nil, err
		}

		res2, err := c.Solve(ctx, gateway.SolveRequest{
			Frontend: "dockerfile.v0",
			FrontendOpt: map[string]string{
				"build-arg:FOO": string(dt[4:8]),
				"target":        "x86target",
			},
		})
		if err != nil {
			return nil, err
		}

		res := gateway.NewResult()
		res.AddRef("linux/arm64", res1.Ref)
		res.AddRef("linux/amd64", res2.Ref)

		pl, err := json.Marshal(exptypes.Platforms{
			Platforms: []exptypes.Platform{
				{
					ID:       "linux/arm64",
					Platform: ocispecs.Platform{OS: "linux", Architecture: "arm64"},
				},
				{
					ID:       "linux/amd64",
					Platform: ocispecs.Platform{OS: "linux", Architecture: "amd64"},
				},
			},
		})
		if err != nil {
			return nil, err
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, pl)

		return res, nil
	}

	_, err = c.Build(sb.Context(), client.SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=full",
		},
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
	}, "", frontend, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 4, len(imgs.Images))

	img := imgs.Find("linux/arm64")
	require.NotNil(t, img)
	require.Equal(t, []byte("bbox\n"), img.Layers[1]["foo"].Data)

	att := imgs.FindAttestation("linux/arm64")
	require.NotNil(t, att)
	require.Equal(t, "attestation-manifest", att.Desc.Annotations["vnd.docker.reference.type"])
	var attest intoto.Statement
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, "https://slsa.dev/provenance/v0.2", attest.PredicateType) // intentionally not const

	type stmtT struct {
		Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
	}
	var stmt stmtT
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
	pred := stmt.Predicate

	require.Equal(t, "https://mobyproject.org/buildkit@v1", pred.BuildType)
	require.Equal(t, "", pred.Builder.ID)
	require.Equal(t, "", pred.Invocation.ConfigSource.URI)

	args := pred.Invocation.Parameters.Args
	require.Equal(t, 2, len(args), "%+v", args)
	require.Equal(t, "The", args["build-arg:FOO"])
	require.Equal(t, "armtarget", args["target"])

	require.Equal(t, 2, len(pred.Invocation.Parameters.Locals))
	require.Equal(t, 1, len(pred.Materials))
	require.Contains(t, pred.Materials[0].URI, "docker/busybox")

	// amd64
	img = imgs.Find("linux/amd64")
	require.NotNil(t, img)
	require.Equal(t, []byte("alpine\n"), img.Layers[1]["foo"].Data)

	att = imgs.FindAttestation("linux/amd64")
	require.NotNil(t, att)
	require.Equal(t, "attestation-manifest", att.Desc.Annotations["vnd.docker.reference.type"])
	attest = intoto.Statement{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, "https://slsa.dev/provenance/v0.2", attest.PredicateType) // intentionally not const

	stmt = stmtT{}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
	pred = stmt.Predicate

	require.Equal(t, "https://mobyproject.org/buildkit@v1", pred.BuildType)
	require.Equal(t, "", pred.Builder.ID)
	require.Equal(t, "", pred.Invocation.ConfigSource.URI)

	args = pred.Invocation.Parameters.Args
	require.Equal(t, 2, len(args), "%+v", args)
	require.Equal(t, "Moby", args["build-arg:FOO"])
	require.Equal(t, "x86target", args["target"])

	require.Equal(t, 2, len(pred.Invocation.Parameters.Locals))
	require.Equal(t, 1, len(pred.Materials))
	require.Contains(t, pred.Materials[0].URI, "docker/alpine")
}

func testClientLLBProvenance(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target := registry + "/buildkit/clientprovenance:llb"

	f := getFrontend(t, sb)

	_, isClient := f.(*clientFrontend)
	if !isClient {
		t.Skip("not a client frontend")
	}

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		st := llb.HTTP("https://raw.githubusercontent.com/moby/moby/v20.10.21/README.md")
		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, err
		}
		// this also shows up in the provenance
		res0, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		dt, err := res0.Ref.ReadFile(ctx, gateway.ReadRequest{
			Filename: "README.md",
		})
		if err != nil {
			return nil, err
		}

		st = llb.Image(integration.UnixOrWindows("alpine", "nanoserver")).File(llb.Mkfile("/foo", 0600, dt))
		def, err = st.Marshal(ctx)
		if err != nil {
			return nil, err
		}
		res1, err := c.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		return res1, nil
	}

	_, err = c.Build(sb.Context(), client.SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=full",
		},
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
		LocalMounts: map[string]fsutil.FS{},
	}, "", frontend, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	nativePlatform := platforms.Format(platforms.Normalize(platforms.DefaultSpec()))

	img := imgs.Find(nativePlatform)
	fileName := integration.UnixOrWindows("foo", "Files/foo")
	require.NotNil(t, img)
	require.Contains(t, string(img.Layers[1][fileName].Data), "The Moby Project")

	att := imgs.FindAttestation(nativePlatform)
	require.NotNil(t, att)
	require.Equal(t, "attestation-manifest", att.Desc.Annotations["vnd.docker.reference.type"])
	var attest intoto.Statement
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &attest))
	require.Equal(t, "https://in-toto.io/Statement/v0.1", attest.Type)
	require.Equal(t, "https://slsa.dev/provenance/v0.2", attest.PredicateType) // intentionally not const

	type stmtT struct {
		Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
	}
	var stmt stmtT
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
	pred := stmt.Predicate

	require.Equal(t, "https://mobyproject.org/buildkit@v1", pred.BuildType)
	require.Equal(t, "", pred.Builder.ID)
	require.Equal(t, "", pred.Invocation.ConfigSource.URI)

	args := pred.Invocation.Parameters.Args
	require.Equal(t, 0, len(args), "%+v", args)
	require.Equal(t, 0, len(pred.Invocation.Parameters.Locals))

	require.Equal(t, 2, len(pred.Materials), "%+v", pred.Materials)
	require.Contains(t, pred.Materials[0].URI, integration.UnixOrWindows("docker/alpine", "docker/nanoserver"))
	require.Contains(t, pred.Materials[1].URI, "README.md")
}

func testSecretSSHProvenance(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	f := getFrontend(t, sb)

	dockerfile := []byte(`
FROM busybox:latest
RUN --mount=type=secret,id=mysecret --mount=type=secret,id=othersecret --mount=type=ssh echo "ok" > /foo
`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	target := registry + "/buildkit/testsecretprovenance:latest"
	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
		},
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	expPlatform := platforms.Format(platforms.Normalize(platforms.DefaultSpec()))

	img := imgs.Find(expPlatform)
	require.NotNil(t, img)
	require.Equal(t, []byte("ok\n"), img.Layers[1]["foo"].Data)

	att := imgs.FindAttestation(expPlatform)
	type stmtT struct {
		Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
	}
	var stmt stmtT
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
	pred := stmt.Predicate

	require.Equal(t, 2, len(pred.Invocation.Parameters.Secrets), "%+v", pred.Invocation.Parameters.Secrets)
	require.Equal(t, "mysecret", pred.Invocation.Parameters.Secrets[0].ID)
	require.True(t, pred.Invocation.Parameters.Secrets[0].Optional)
	require.Equal(t, "othersecret", pred.Invocation.Parameters.Secrets[1].ID)
	require.True(t, pred.Invocation.Parameters.Secrets[1].Optional)

	require.Equal(t, 1, len(pred.Invocation.Parameters.SSH), "%+v", pred.Invocation.Parameters.SSH)
	require.Equal(t, "default", pred.Invocation.Parameters.SSH[0].ID)
	require.True(t, pred.Invocation.Parameters.SSH[0].Optional)
}

func testOCILayoutProvenance(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)
	target := registry + "/buildkit/clientprovenance:ocilayout"

	f := getFrontend(t, sb)
	_, isGateway := f.(*gatewayFrontend)

	ocidir := t.TempDir()
	ociDockerfile := []byte(`
FROM scratch
COPY <<EOF /foo
foo
EOF
	`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", ociDockerfile, 0600),
	)

	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		Exports: []client.ExportEntry{
			{
				Type:      client.ExporterOCI,
				OutputDir: ocidir,
				Attrs: map[string]string{
					"tar": "false",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	var index ocispecs.Index
	dt, err := os.ReadFile(filepath.Join(ocidir, ocispecs.ImageIndexFile))
	require.NoError(t, err)
	err = json.Unmarshal(dt, &index)
	require.NoError(t, err)
	require.Equal(t, 1, len(index.Manifests))
	digest := index.Manifests[0].Digest.Hex()

	store, err := local.NewStore(ocidir)
	require.NoError(t, err)
	ociID := "ocione"

	dockerfile := []byte(`
FROM foo
COPY <<EOF /bar
bar
EOF
`)
	dir = integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"context:foo":       fmt.Sprintf("oci-layout:%s@sha256:%s", ociID, digest),
			"attest:provenance": "mode=max",
		},
		OCIStores: map[string]content.Store{
			ociID: store,
		},
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	expPlatform := platforms.Format(platforms.Normalize(platforms.DefaultSpec()))

	img := imgs.Find(expPlatform)
	require.NotNil(t, img)
	require.Equal(t, []byte("foo\n"), img.Layers[0]["foo"].Data)
	require.Equal(t, []byte("bar\n"), img.Layers[1]["bar"].Data)

	att := imgs.FindAttestation(expPlatform)
	type stmtT struct {
		Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
	}
	var stmt stmtT
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
	pred := stmt.Predicate

	if isGateway {
		require.Len(t, pred.Materials, 2)
	} else {
		require.Len(t, pred.Materials, 1)
	}
	var material *provenanceCommon.ProvenanceMaterial
	for _, m := range pred.Materials {
		if strings.Contains(m.URI, "/foo") {
			require.Nil(t, material, pred.Materials)
			material = &m
		}
	}
	require.NotNil(t, material)
	prefix, _, _ := strings.Cut(material.URI, "/")
	require.Equal(t, "pkg:oci", prefix)
	require.Equal(t, digest, material.Digest["sha256"])
}

func testNilProvenance(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	f := getFrontend(t, sb)

	dockerfile := []byte(integration.UnixOrWindows(
		`
FROM scratch
ENV FOO=bar
`,
		`
FROM scratch
ENV FOO=bar
`,
	))
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)
	buf := &bytes.Buffer{}

	exporters := []struct {
		name   string
		export client.ExportEntry
	}{
		{
			name: "image",
			export: client.ExportEntry{
				Type: client.ExporterImage,
			},
		},
		{
			name: "local",
			export: client.ExportEntry{
				Type:      client.ExporterLocal,
				OutputDir: t.TempDir(),
			},
		},
		{
			name: "tar",
			export: func() client.ExportEntry {
				return client.ExportEntry{
					Type:   client.ExporterTar,
					Output: fixedWriteCloser(&nopWriteCloser{buf}),
				}
			}(),
		},
	}

	for _, exp := range exporters {
		for _, platformMode := range []string{"single", "multi"} {
			t.Run(exp.name+"/"+platformMode, func(t *testing.T) {
				attrs := map[string]string{
					"attest:provenance": "mode=max",
				}
				if platformMode == "multi" {
					attrs["platform"] = "linux/amd64,linux/arm64"
				}
				_, err = f.Solve(sb.Context(), c, client.SolveOpt{
					LocalMounts: map[string]fsutil.FS{
						dockerui.DefaultLocalNameDockerfile: dir,
						dockerui.DefaultLocalNameContext:    dir,
					},
					FrontendAttrs: attrs,
					Exports: []client.ExportEntry{
						exp.export,
					},
				}, nil)
				require.NoError(t, err)
			})
		}
	}
}

// https://github.com/moby/buildkit/issues/3562
func testDuplicatePlatformProvenance(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	f := getFrontend(t, sb)

	dockerfile := []byte(
		`
FROM alpine as base-linux
FROM nanoserver as base-windows
FROM base-$TARGETOS
`,
	)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)
	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
			"platform":          "linux/amd64,linux/amd64",
		},
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
	}, nil)
	require.NoError(t, err)
}

// https://github.com/moby/buildkit/issues/3928
func testDockerIgnoreMissingProvenance(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureProvenance)
	c, err := client.New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dockerfile := []byte(integration.UnixOrWindows(
		`
FROM alpine
`,
		`
FROM nanoserver
`,
	))
	dirDockerfile := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)
	dirContext := integration.Tmpdir(t)

	frontend := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		// remove the directory to simulate the case where the context
		// directory does not exist, and either no validation checks were run,
		// or they passed erroneously
		if err := os.RemoveAll(dirContext.Name); err != nil {
			return nil, err
		}

		res, err := c.Solve(ctx, gateway.SolveRequest{
			Frontend: "dockerfile.v0",
		})
		if err != nil {
			return nil, err
		}
		return res, nil
	}

	_, err = c.Build(sb.Context(), client.SolveOpt{
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
		},
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dirDockerfile,
			dockerui.DefaultLocalNameContext:    dirContext,
		},
	}, "", frontend, nil)
	require.NoError(t, err)
}

func testCommandSourceMapping(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dockerfile := []byte(`FROM alpine
RUN echo "hello" > foo
WORKDIR /tmp
COPY foo foo2
COPY --link foo foo3
ADD bar bar`)

	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
		fstest.CreateFile("foo", []byte("data"), 0600),
		fstest.CreateFile("bar", []byte("data2"), 0600),
	)

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target := registry + "/buildkit/testsourcemappingprov:latest"
	f := getFrontend(t, sb)

	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
		},
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	expPlatform := platforms.Format(platforms.Normalize(platforms.DefaultSpec()))

	img := imgs.Find(expPlatform)
	require.NotNil(t, img)

	att := imgs.FindAttestation(expPlatform)
	type stmtT struct {
		Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
	}
	var stmt stmtT
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
	pred := stmt.Predicate

	def := pred.BuildConfig.Definition

	steps := map[string]provenancetypes.BuildStep{}
	for _, step := range def {
		steps[step.ID] = step
	}
	// ensure all IDs are unique
	require.Equal(t, len(steps), len(def))

	src := pred.Metadata.BuildKitMetadata.Source

	lines := make([]bool, bytes.Count(dockerfile, []byte("\n"))+1)

	for id, loc := range src.Locations {
		// - only context upload can be without source mapping
		// - every step must only be in one line
		// - perform bounds check for location
		step, ok := steps[id]
		require.True(t, ok, "definition for step %s not found", id)

		if len(loc.Locations) == 0 {
			s := step.Op.GetSource()
			require.NotNil(t, s, "unmapped step %s is not source", id)
			require.Equal(t, "local://context", s.Identifier)
		} else if len(loc.Locations) >= 1 {
			require.Equal(t, 1, len(loc.Locations), "step %s has more than one location", id)
		}

		for _, loc := range loc.Locations {
			for _, r := range loc.Ranges {
				require.Equal(t, r.Start.Line, r.End.Line, "step %s has range with multiple lines", id)

				idx := r.Start.Line - 1
				if idx < 0 || int(idx) >= len(lines) {
					t.Fatalf("step %s has invalid range on line %d", id, idx)
				}
				lines[idx] = true
			}
		}
	}

	// ensure all lines are covered
	for i, covered := range lines {
		require.True(t, covered, "line %d is not covered", i+1)
	}
}

func testFrontendDeduplicateSources(t *testing.T, sb integration.Sandbox) {
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	dockerfile := fmt.Appendf(nil,
		`
FROM %s as base
COPY foo foo2

FROM linked
COPY bar bar2
`,
		integration.UnixOrWindows("scratch", "nanoserver"),
	)

	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
		fstest.CreateFile("foo", []byte("data"), 0600),
		fstest.CreateFile("bar", []byte("data2"), 0600),
	)

	f := getFrontend(t, sb)

	b := func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		res, err := f.SolveGateway(ctx, c, gateway.SolveRequest{
			FrontendOpt: map[string]string{
				"target": "base",
			},
		})
		if err != nil {
			return nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}
		st, err := ref.ToState()
		if err != nil {
			return nil, err
		}

		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, err
		}

		dt, ok := res.Metadata["containerimage.config"]
		if !ok {
			return nil, errors.Errorf("no containerimage.config in metadata")
		}

		dt, err = json.Marshal(map[string][]byte{
			"containerimage.config": dt,
		})
		if err != nil {
			return nil, err
		}

		res, err = f.SolveGateway(ctx, c, gateway.SolveRequest{
			FrontendOpt: map[string]string{
				"context:linked":        "input:baseinput",
				"input-metadata:linked": string(dt),
			},
			FrontendInputs: map[string]*pb.Definition{
				"baseinput": def.ToPB(),
			},
		})
		if err != nil {
			return nil, err
		}
		return res, nil
	}

	product := "buildkit_test"

	destDir := t.TempDir()

	ref := identity.NewID()

	_, err = c.Build(ctx, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		Exports: []client.ExportEntry{
			{
				Type:      client.ExporterLocal,
				OutputDir: destDir,
			},
		},
		Ref: ref,
	}, product, b, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "foo2"))
	require.NoError(t, err)
	require.Equal(t, "data", string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "bar2"))
	require.NoError(t, err)
	require.Equal(t, "data2", string(dt))

	history, err := c.ControlClient().ListenBuildHistory(ctx, &controlapi.BuildHistoryRequest{
		Ref:       ref,
		EarlyExit: true,
	})
	require.NoError(t, err)

	store := proxy.NewContentStore(c.ContentClient())

	var provDt []byte
	for {
		ev, err := history.Recv()
		if err != nil {
			require.Equal(t, io.EOF, err)
			break
		}
		require.Equal(t, ref, ev.Record.Ref)
		require.Len(t, ev.Record.Exporters, 1)

		for _, prov := range ev.Record.Result.Attestations {
			if len(prov.Annotations) == 0 || prov.Annotations["in-toto.io/predicate-type"] != "https://slsa.dev/provenance/v0.2" {
				t.Logf("skipping non-slsa provenance: %s", prov.MediaType)
				continue
			}

			provDt, err = content.ReadBlob(ctx, store, ocispecs.Descriptor{
				MediaType: prov.MediaType,
				Digest:    digest.Digest(prov.Digest),
				Size:      prov.Size,
			})
			require.NoError(t, err)
		}
	}

	require.NotEqual(t, 0, len(provDt))

	var pred provenancetypes.ProvenancePredicateSLSA02
	require.NoError(t, json.Unmarshal(provDt, &pred))

	sources := pred.Metadata.BuildKitMetadata.Source.Infos

	require.Equal(t, 1, len(sources))
	require.Equal(t, "Dockerfile", sources[0].Filename)
	require.Equal(t, "Dockerfile", sources[0].Language)

	require.Equal(t, dockerfile, sources[0].Data)
	require.NotEqual(t, 0, len(sources[0].Definition))
}

func testDuplicateLayersProvenance(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	f := getFrontend(t, sb)

	// Create a triangle shape with the layers.
	// This will trigger the provenance attestation to attempt to add the base
	// layer multiple times.
	dockerfile := []byte(`
FROM busybox:latest AS base

FROM base AS a
RUN date +%s > /a.txt

FROM base AS b
COPY --from=a /a.txt /
RUN date +%s > /b.txt
`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	target := registry + "/buildkit/testwithprovenance:dup"

	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
			"filename":          "Dockerfile",
		},
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterImage,
				Attrs: map[string]string{
					"name": target,
					"push": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	desc, provider, err := contentutil.ProviderFromRef(target)
	require.NoError(t, err)
	imgs, err := testutil.ReadImages(sb.Context(), provider, desc)
	require.NoError(t, err)
	require.Equal(t, 2, len(imgs.Images))

	att := imgs.Find("unknown/unknown")
	require.NotNil(t, att)

	var stmt struct {
		Predicate provenancetypes.ProvenancePredicateSLSA02 `json:"predicate"`
	}
	require.NoError(t, json.Unmarshal(att.LayersRaw[0], &stmt))
	pred := stmt.Predicate

	// Search for the layer list for step0.
	metadata := pred.Metadata
	require.NotNil(t, metadata)

	layers := metadata.BuildKitMetadata.Layers["step0:0"]
	require.NotNil(t, layers)
	require.Len(t, layers, 1)
}

func testProvenanceExportLocal(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	f := getFrontend(t, sb)

	dockerfile := []byte(`
FROM busybox:latest AS base
COPY <<EOF /out/foo
ok
EOF

FROM scratch
COPY --from=base /out /
`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	destDir := t.TempDir()
	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
		},
		Exports: []client.ExportEntry{
			{
				Type:      client.ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	dt, err := os.ReadFile(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, "ok\n", string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, "provenance.json"))
	require.NoError(t, err)
	require.NotEqual(t, 0, len(dt))

	var pred provenancetypes.ProvenancePredicateSLSA02
	require.NoError(t, json.Unmarshal(dt, &pred))
}

func testProvenanceExportLocalForceSplit(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	f := getFrontend(t, sb)

	dockerfile := []byte(`
FROM busybox:latest AS base
COPY <<EOF /out/foo
ok
EOF

FROM scratch
COPY --from=base /out /
`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	destDir := t.TempDir()
	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
		},
		Exports: []client.ExportEntry{
			{
				Type:      client.ExporterLocal,
				OutputDir: destDir,
				Attrs: map[string]string{
					"platform-split": "true",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	expPlatform := strings.ReplaceAll(platforms.FormatAll(platforms.DefaultSpec()), "/", "_")

	dt, err := os.ReadFile(filepath.Join(destDir, expPlatform, "foo"))
	require.NoError(t, err)
	require.Equal(t, "ok\n", string(dt))

	dt, err = os.ReadFile(filepath.Join(destDir, expPlatform, "provenance.json"))
	require.NoError(t, err)
	require.NotEqual(t, 0, len(dt))

	var pred provenancetypes.ProvenancePredicateSLSA02
	require.NoError(t, json.Unmarshal(dt, &pred))
}

func testProvenanceExportLocalMultiPlatform(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureMultiPlatform, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	f := getFrontend(t, sb)

	dockerfile := []byte(`
FROM busybox:latest AS base
COPY <<EOF /out/foo
ok
EOF

FROM scratch
COPY --from=base /out /
`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	destDir := t.TempDir()
	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
			"platform":          "linux/amd64,linux/arm64",
		},
		Exports: []client.ExportEntry{
			{
				Type:      client.ExporterLocal,
				OutputDir: destDir,
			},
		},
	}, nil)
	require.NoError(t, err)

	for _, platform := range []string{"linux_amd64", "linux_arm64"} {
		dt, err := os.ReadFile(filepath.Join(destDir, platform, "foo"))
		require.NoError(t, err)
		require.Equal(t, "ok\n", string(dt))

		dt, err = os.ReadFile(filepath.Join(destDir, platform, "provenance.json"))
		require.NoError(t, err)
		require.NotEqual(t, 0, len(dt))

		var pred provenancetypes.ProvenancePredicateSLSA02
		require.NoError(t, json.Unmarshal(dt, &pred))
	}
}

func testProvenanceExportLocalMultiPlatformNoSplit(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	workers.CheckFeatureCompat(t, sb, workers.FeatureMultiPlatform, workers.FeatureProvenance)
	ctx := sb.Context()

	c, err := client.New(ctx, sb.Address())
	require.NoError(t, err)
	defer c.Close()

	f := getFrontend(t, sb)

	dockerfile := []byte(`
FROM busybox:latest AS base
ARG TARGETARCH
COPY <<EOF /out/foo_${TARGETARCH}
ok
EOF

FROM scratch
COPY --from=base /out /
`)
	dir := integration.Tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
	)

	destDir := t.TempDir()
	_, err = f.Solve(sb.Context(), c, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			dockerui.DefaultLocalNameDockerfile: dir,
			dockerui.DefaultLocalNameContext:    dir,
		},
		FrontendAttrs: map[string]string{
			"attest:provenance": "mode=max",
			"platform":          "linux/amd64,linux/arm64",
		},
		Exports: []client.ExportEntry{
			{
				Type:      client.ExporterLocal,
				OutputDir: destDir,
				Attrs: map[string]string{
					"platform-split": "false",
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	for _, arch := range []string{"amd64", "arm64"} {
		dt, err := os.ReadFile(filepath.Join(destDir, "foo_"+arch))
		require.NoError(t, err)
		require.Equal(t, "ok\n", string(dt))

		dt, err = os.ReadFile(filepath.Join(destDir, "provenance.linux_"+arch+".json"))
		require.NoError(t, err)
		require.NotEqual(t, 0, len(dt))

		var pred provenancetypes.ProvenancePredicateSLSA02
		require.NoError(t, json.Unmarshal(dt, &pred))
	}
}
