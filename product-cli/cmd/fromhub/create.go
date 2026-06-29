package fromhub

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	"github.com/openshift/hypershift/support/config"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	// LabelFromHubCLI marks every resource created via `hcp from-hub create`.
	LabelFromHubCLI      = "hcp.ocm.io/created-by"
	LabelFromHubCLIValue = "from-hub-cli"

	// LabelHostedCluster records the owning HostedCluster name on every related resource.
	LabelHostedCluster = "hcp.ocm.io/hostedcluster"
)

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

	cmd.AddCommand(newCreateAWSCommand(hubOpts, coreOpts))
	cmd.AddCommand(newCreateAzureCommand(hubOpts, coreOpts))
	cmd.AddCommand(newCreateAgentCommand(hubOpts, coreOpts))
	cmd.AddCommand(newCreateKubevirtCommand(hubOpts, coreOpts))
	cmd.AddCommand(newCreateOpenStackCommand(hubOpts, coreOpts))

	return cmd
}

func newCreateAWSCommand(hubOpts *Options, coreOpts *core.RawCreateOptions) *cobra.Command {
	coreOpts.ReleaseStream = config.DefaultReleaseStream
	awsOpts := hypershiftaws.DefaultOptions()

	cmd := &cobra.Command{
		Use:          "aws",
		Short:        "Create a HostedCluster on AWS via the hub HCP proxy",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runViaProxy(cmd, hubOpts, coreOpts, awsOpts)
		},
	}
	hypershiftaws.BindOptions(awsOpts, cmd.Flags())
	return cmd
}

func newCreateAzureCommand(hubOpts *Options, coreOpts *core.RawCreateOptions) *cobra.Command {
	coreOpts.ReleaseStream = config.DefaultReleaseStream
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
func buildRequestFromFile(filename string) (CreateRequest, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return CreateRequest{}, fmt.Errorf("read %s: %w", filename, err)
	}

	var (
		hc      *hypershiftv1beta1.HostedCluster
		nps     []*hypershiftv1beta1.NodePool
		secrets []corev1.Secret
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
		var errBody map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return fmt.Errorf("proxy returned %d: %v", resp.StatusCode, errBody)
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
