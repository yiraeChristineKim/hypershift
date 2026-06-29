package fromhub

import "github.com/spf13/cobra"

// Options holds the shared flags for all hcp from-hub subcommands.
type Options struct {
	Kubeconfig     string
	HostingCluster string
	Namespace      string
	Context        string

	// ProxyURL bypasses the hub kube-apiserver and talks directly to the HCP
	// proxy (e.g. https://localhost:8443 via kubectl port-forward).
	// TLS verification is skipped when this flag is set.
	// For local development / testing only.
	ProxyURL string

	// ImpersonateUser and ImpersonateGroups inject X-Remote-User / X-Remote-Group
	// headers when using --proxy-url. In production these headers are set by the
	// hub kube-apiserver; these flags let you simulate them for local testing.
	ImpersonateUser   string
	ImpersonateGroups []string
}

// NewCommand returns the cobra.Command for 'hcp from-hub'.
func NewCommand() *cobra.Command {
	opts := &Options{Namespace: "clusters"}

	cmd := &cobra.Command{
		Use:          "from-hub",
		Short:        "Manage HostedClusters via the hub proxy API (hcp.ocm.io/v1alpha1)",
		SilenceUsage: true,
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	// Persistent flags scoped to this group only — not on the root hcp command
	f := cmd.PersistentFlags()
	f.StringVar(&opts.Kubeconfig, "hub-kubeconfig", "",
		"Path to hub kubeconfig (defaults to $KUBECONFIG or ~/.kube/config)")
	f.StringVar(&opts.HostingCluster, "hosting-cluster", "",
		"Name of the hosting ManagedCluster (required for most commands)")
	f.StringVar(&opts.Namespace, "namespace", "clusters",
		"Namespace for HostedCluster resources")
	f.StringVar(&opts.Context, "context", "",
		"Kubeconfig context to use")
	f.StringVar(&opts.ProxyURL, "proxy-url", "",
		"Connect directly to the HCP proxy (e.g. https://localhost:8443). Skips hub kube-apiserver auth. For local testing only.")
	f.StringVar(&opts.ImpersonateUser, "as", "",
		"Impersonate this user (sets X-Remote-User header). Only effective with --proxy-url.")
	f.StringArrayVar(&opts.ImpersonateGroups, "as-group", nil,
		"Impersonate this group (sets X-Remote-Group header). Can be specified multiple times. Only effective with --proxy-url.")

	cmd.AddCommand(newCreateCommand(opts))
	cmd.AddCommand(newDeleteCommand(opts))
	cmd.AddCommand(newEditCommand(opts))

	return cmd
}
