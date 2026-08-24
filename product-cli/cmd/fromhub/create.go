package fromhub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hcpagent "github.com/openshift/hypershift/cmd/cluster/agent"
	hypershiftaws "github.com/openshift/hypershift/cmd/cluster/aws"
	hypershiftazure "github.com/openshift/hypershift/cmd/cluster/azure"
	"github.com/openshift/hypershift/cmd/cluster/core"
	hcpkubevirt "github.com/openshift/hypershift/cmd/cluster/kubevirt"
	hcpopenstack "github.com/openshift/hypershift/cmd/cluster/openstack"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/yaml"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	// LabelFromHubCLI marks every resource created via `hcp from-hub create`.
	LabelFromHubCLI      = "hcp.ocm.io/created-by"
	LabelFromHubCLIValue = "from-hub-cli"

	// LabelHostedCluster records the owning HostedCluster name on every related resource.
	LabelHostedCluster = "hcp.ocm.io/hostedcluster"
)

// unsupportedFlag names a flag from-hub inherits from product-cli (via
// core.BindOptions or a platform's own BindOptions) that is meaningless,
// misleading, or actively wrong in the from-hub flow, along with why.
type unsupportedFlag struct {
	name   string
	reason string
}

// unsupportedFlags are inherited from `core.BindOptions` (shared with
// `hcp create cluster`) but are meaningless, misleading, or actively wrong
// for `hcp from-hub create`: it always renders locally and creates resources
// via the HCP proxy, so it never applies resources directly, never blocks
// waiting for cluster readiness, and never checks the CLI version against the
// hub's HyperShift Operator as if the hub were the hosting cluster.
var unsupportedFlags = []unsupportedFlag{
	{"version-check", "compares the CLI against the hub HyperShift Operator version, not the hosting cluster's, which is misleading; not supported for from-hub"},
	{"wait", "from-hub create always renders and POSTs via the HCP proxy; it never blocks waiting for cluster readiness"},
	{"timeout", "has no effect without --wait, which is not supported for from-hub"},
	{"render", "from-hub create always renders internally before posting to the proxy; standalone rendering is not supported"},
	{"render-into", "from-hub create always renders into a temporary file before posting to the proxy; a custom render destination is not supported"},
	{"render-sensitive", "from-hub create always renders secrets internally so they can be posted to the proxy; this flag has no additional effect"},
}

// awsUnsupportedFlags apply only to `hcp from-hub create aws`.
var awsUnsupportedFlags = []unsupportedFlag{
	{"secret-creds", "reads AWS credentials from a Kubernetes Secret via the default kubeconfig context, which is the hub for from-hub, not the hosting cluster, so the Secret is either missing or belongs to the wrong cluster; use --pull-secret, --sts-creds, and --role-arn (local files) instead"},
}

// hideFlags hides the given flags on flags, so `--help` does not advertise
// behavior from-hub cannot provide. The flags still parse (so rejectFlags can
// give a clear error if a user sets one explicitly, e.g. from a script or
// muscle memory), they are just no longer discoverable via --help.
func hideFlags(flags *pflag.FlagSet, unsupported []unsupportedFlag) {
	for _, f := range unsupported {
		_ = flags.MarkHidden(f.name)
	}
}

// rejectFlags returns an error if the caller explicitly set any flag in
// unsupported, instead of silently no-op'ing (--wait/--timeout/--render*) or
// silently doing the wrong thing (--version-check / --secret-creds against
// the hub).
func rejectFlags(cmd *cobra.Command, unsupported []unsupportedFlag) error {
	for _, f := range unsupported {
		if cmd.Flags().Changed(f.name) {
			return fmt.Errorf("--%s is not supported for `hcp from-hub create`: %s", f.name, f.reason)
		}
	}
	return nil
}

// newCreateCommand returns `hcp from-hub create` with one subcommand per platform.
// Each subcommand accepts the same flags as the corresponding `hcp create cluster <platform>`
// command, but routes the rendered resources through the HCP proxy instead of applying them
// directly to the hub cluster.
func newCreateCommand(hubOpts *Options) *cobra.Command {
	coreOpts := core.DefaultOptions()
	coreOpts.ControlPlaneAvailabilityPolicy = string(hypershiftv1beta1.HighlyAvailable)

	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a HostedCluster via the hub HCP proxy",
		Long:         "Render HostedCluster resources for the chosen platform and apply them to the hosting cluster via the hypershift-addon-operator HCP proxy.",
		SilenceUsage: true,
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	// Shared core flags (--name, --release-image, --pull-secret, --namespace, …)
	core.BindOptions(coreOpts, cmd.PersistentFlags())

	// core.BindOptions also binds flags that are misleading or no-ops for
	// from-hub (--version-check, --wait, --timeout, --render*); hide them from
	// --help and reject explicit use in runViaProxy. See unsupportedFlags.
	hideFlags(cmd.PersistentFlags(), unsupportedFlags)

	// Unlike `hcp create cluster`, from-hub has no local kubeconfig access to the
	// hosting cluster to infer a base domain (e.g. KubeVirt's baseDomainPassthrough
	// or Agent's "example.com" default), and any such inference would incorrectly
	// read from the hub instead. Always require an explicit value.
	_ = cmd.MarkPersistentFlagRequired("base-domain")

	cmd.AddCommand(newCreateAWSCommand(hubOpts, coreOpts))
	cmd.AddCommand(newCreateAzureCommand(hubOpts, coreOpts))
	cmd.AddCommand(newCreateAgentCommand(hubOpts, coreOpts))
	cmd.AddCommand(newCreateKubevirtCommand(hubOpts, coreOpts))
	cmd.AddCommand(newCreateOpenStackCommand(hubOpts, coreOpts))

	return cmd
}

func newCreateAWSCommand(hubOpts *Options, coreOpts *core.RawCreateOptions) *cobra.Command {
	awsOpts := hypershiftaws.DefaultOptions()

	cmd := &cobra.Command{
		Use:          "aws",
		Short:        "Create a HostedCluster on AWS via the hub HCP proxy",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectFlags(cmd, awsUnsupportedFlags); err != nil {
				return err
			}
			return runViaProxy(cmd, hubOpts, coreOpts, awsOpts)
		},
	}
	hypershiftaws.BindOptions(awsOpts, cmd.Flags())

	// --secret-creds reads a Secret via the default kubeconfig context, which
	// is wrong for from-hub; hide it from --help (see awsUnsupportedFlags),
	// RunE rejects it if the caller sets it explicitly.
	hideFlags(cmd.Flags(), awsUnsupportedFlags)
	return cmd
}

func newCreateAzureCommand(hubOpts *Options, coreOpts *core.RawCreateOptions) *cobra.Command {
	azureOpts := hypershiftazure.DefaultOptions()

	cmd := &cobra.Command{
		Use:          "azure",
		Short:        "Create a HostedCluster on Azure via the hub HCP proxy",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runViaProxy(cmd, hubOpts, coreOpts, azureOpts)
		},
	}
	hypershiftazure.BindProductFlags(azureOpts, cmd.Flags())
	hypershiftazure.BindProductCoreFlags(coreOpts, cmd.Flags())
	return cmd
}

func newCreateAgentCommand(hubOpts *Options, coreOpts *core.RawCreateOptions) *cobra.Command {
	agentOpts := hcpagent.DefaultOptions()

	cmd := &cobra.Command{
		Use:          "agent",
		Short:        "Create a HostedCluster on Agent platform via the hub HCP proxy",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runViaProxy(cmd, hubOpts, coreOpts, agentOpts)
		},
	}
	hcpagent.BindOptions(agentOpts, cmd.Flags())
	_ = cmd.MarkFlagRequired("agent-namespace")
	return cmd
}

func newCreateKubevirtCommand(hubOpts *Options, coreOpts *core.RawCreateOptions) *cobra.Command {
	kvOpts := hcpkubevirt.DefaultOptions()

	cmd := &cobra.Command{
		Use:          "kubevirt",
		Short:        "Create a HostedCluster on KubeVirt via the hub HCP proxy",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runViaProxy(cmd, hubOpts, coreOpts, kvOpts)
		},
	}
	hcpkubevirt.BindOptions(kvOpts, cmd.Flags())
	return cmd
}

func newCreateOpenStackCommand(hubOpts *Options, coreOpts *core.RawCreateOptions) *cobra.Command {
	osOpts := hcpopenstack.DefaultOptions()

	cmd := &cobra.Command{
		Use:          "openstack",
		Short:        "Create a HostedCluster on OpenStack via the hub HCP proxy",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runViaProxy(cmd, hubOpts, coreOpts, osOpts)
		},
	}
	hcpopenstack.BindOptions(osOpts, cmd.Flags())
	return cmd
}

// runViaProxy renders resources using core.CreateCluster (render mode) and
// POSTs the result to the hosting cluster via the HCP proxy.
func runViaProxy(cmd *cobra.Command, hubOpts *Options, coreOpts *core.RawCreateOptions, platformOpts core.PlatformValidator) error {
	if err := rejectFlags(cmd, unsupportedFlags); err != nil {
		return err
	}

	if hubOpts.HostingCluster == "" {
		return fmt.Errorf("--hosting-cluster is required")
	}

	// Write rendered YAML to a temp file; core.CreateCluster writes there.
	tmp, err := os.CreateTemp("", "hcp-render-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	coreOpts.Render = true
	coreOpts.RenderSensitive = true
	coreOpts.RenderInto = tmp.Name()

	if err := core.CreateCluster(cmd.Context(), coreOpts, platformOpts); err != nil {
		return fmt.Errorf("render resources: %w", err)
	}

	req, err := buildRequestFromFile(tmp.Name())
	if err != nil {
		return err
	}

	stampFromHubLabels(&req)
	return postToProxy(cmd, hubOpts, req)
}

// buildRequestFromFile parses a multi-document YAML file rendered by
// `hcp create cluster --render --render-sensitive` and returns a CreateRequest.
// Secret objects are passed through as-is so the server creates them verbatim
// on the spoke (pull-secret, ssh-key, cloud-provider credential secrets, …).
// Any object whose Kind isn't HostedCluster/NodePool/Secret (e.g. Agent's
// capi-provider-role Role, the user-ca-bundle ConfigMap) is preserved in
// ExtraObjects instead of being silently dropped.
func buildRequestFromFile(filename string) (CreateRequest, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return CreateRequest{}, fmt.Errorf("read %s: %w", filename, err)
	}

	var (
		hc      *hypershiftv1beta1.HostedCluster
		nps     []*hypershiftv1beta1.NodePool
		secrets []corev1.Secret
		extras  []runtime.RawExtension
	)

	for _, doc := range splitYAMLDocs(data) {
		var typeMeta metav1.TypeMeta
		if err := yaml.Unmarshal(doc, &typeMeta); err != nil || typeMeta.Kind == "" {
			continue
		}

		switch typeMeta.Kind {
		case "HostedCluster":
			hc = &hypershiftv1beta1.HostedCluster{}
			if err := yaml.Unmarshal(doc, hc); err != nil {
				return CreateRequest{}, fmt.Errorf("parse HostedCluster: %w", err)
			}
		case "NodePool":
			np := &hypershiftv1beta1.NodePool{}
			if err := yaml.Unmarshal(doc, np); err != nil {
				return CreateRequest{}, fmt.Errorf("parse NodePool: %w", err)
			}
			nps = append(nps, np)
		case "Secret":
			var secret corev1.Secret
			if err := yaml.Unmarshal(doc, &secret); err != nil {
				return CreateRequest{}, fmt.Errorf("parse Secret: %w", err)
			}
			secrets = append(secrets, secret)
		default:
			jsonDoc, err := yaml.YAMLToJSON(doc)
			if err != nil {
				return CreateRequest{}, fmt.Errorf("parse %s: %w", typeMeta.Kind, err)
			}
			extras = append(extras, runtime.RawExtension{Raw: jsonDoc})
		}
	}

	if hc == nil {
		return CreateRequest{}, fmt.Errorf("no HostedCluster found in rendered output")
	}
	if len(secrets) == 0 {
		return CreateRequest{}, fmt.Errorf("no Secrets found in rendered output; ensure --pull-secret and --render-sensitive are set")
	}

	return CreateRequest{
		HostedCluster: hc,
		NodePools:     nps,
		Secrets:       secrets,
		ExtraObjects:  extras,
	}, nil
}

// postToProxy marshals req and POSTs it to the HCP proxy via the hub kube-apiserver.
func postToProxy(cmd *cobra.Command, opts *Options, req CreateRequest) error {
	ns := opts.Namespace
	if req.HostedCluster != nil && req.HostedCluster.Namespace != "" {
		ns = req.HostedCluster.Namespace
	}

	httpClient, host, err := buildHTTPClient(opts)
	if err != nil {
		return err
	}

	url := hostedClusterURL(host, ns, "", opts.HostingCluster)
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST to proxy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("proxy returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	fmt.Fprintf(cmd.OutOrStdout(), "HostedCluster %q created on %s\n", req.HostedCluster.Name, opts.HostingCluster)
	return nil
}

// stampFromHubLabels adds hcp.ocm.io/created-by and hcp.ocm.io/hostedcluster
// to every resource in the request.
// The Namespace gets the same labels server-side (see buildNamespace in hcp_proxy.go).
func stampFromHubLabels(req *CreateRequest) {
	hcName := ""
	if req.HostedCluster != nil {
		hcName = req.HostedCluster.Name
	}

	addLabels := func(l map[string]string) map[string]string {
		if l == nil {
			l = map[string]string{}
		}
		l[LabelFromHubCLI] = LabelFromHubCLIValue
		if hcName != "" {
			l[LabelHostedCluster] = hcName
		}
		return l
	}

	if req.HostedCluster != nil {
		req.HostedCluster.Labels = addLabels(req.HostedCluster.Labels)
	}
	for i := range req.NodePools {
		if req.NodePools[i] != nil {
			req.NodePools[i].Labels = addLabels(req.NodePools[i].Labels)
		}
	}
	for i := range req.Secrets {
		req.Secrets[i].Labels = addLabels(req.Secrets[i].Labels)
	}
	for i := range req.ExtraObjects {
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(req.ExtraObjects[i].Raw, u); err != nil {
			// Best-effort labeling; the proxy still receives the object unlabeled.
			continue
		}
		u.SetLabels(addLabels(u.GetLabels()))
		if raw, err := json.Marshal(u); err == nil {
			req.ExtraObjects[i].Raw = raw
		}
	}
}

// splitYAMLDocs splits a multi-document YAML byte slice on "---" boundaries.
func splitYAMLDocs(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range strings.Split(string(data), "\n---") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			docs = append(docs, []byte(trimmed))
		}
	}
	return docs
}
