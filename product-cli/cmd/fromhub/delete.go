package fromhub

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

func newDeleteCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "delete <name>",
		Short:        "Delete a HostedCluster via the hub proxy API",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd, opts, args[0])
		},
	}
	return cmd
}

func runDelete(_ *cobra.Command, opts *Options, name string) error {
	if opts.HostingCluster == "" {
		return fmt.Errorf("--hosting-cluster is required")
	}

	httpClient, host, err := buildHTTPClient(opts)
	if err != nil {
		return err
	}

	url := hostedClusterURL(host, opts.Namespace, name, opts.HostingCluster)
	req, err := newRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}

	fmt.Printf("HostedCluster %q deleted from %s\n", name, opts.HostingCluster)
	return nil
}
