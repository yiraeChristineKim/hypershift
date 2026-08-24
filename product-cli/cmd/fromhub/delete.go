package fromhub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/cmd/cluster/core"
	awsinfra "github.com/openshift/hypershift/cmd/infra/aws"
	"github.com/openshift/hypershift/cmd/log"
	"github.com/openshift/hypershift/cmd/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spf13/cobra"
)

func newDeleteCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "delete <name>",
		Short:        "Delete a HostedCluster (and optionally its cloud infrastructure)",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd, opts, args[0])
		},
	}
	cmd.AddCommand(newDeleteAWSCommand(opts))
	return cmd
}

func newDeleteAWSCommand(hubOpts *Options) *cobra.Command {
	destroyOpts := &core.DestroyOptions{
		AWSPlatform: core.AWSPlatformDestroyOptions{
			Region: "us-east-1",
		},
	}

	cmd := &cobra.Command{
		Use:   "aws <name>",
		Short: "Delete a HostedCluster and destroy its AWS infrastructure",
		Long: `Destroy all AWS infrastructure (VPC, subnets, NAT gateway, EIP, IAM roles,
OIDC provider, Route53 zones, etc.) and then delete the HostedCluster via the
hub HCP proxy. Infrastructure is destroyed first so that if teardown fails the
HostedCluster remains intact on the hosting cluster and the operation can be
retried. AWS parameters (infra-id, region, base-domain) are read automatically
from the live HostedCluster; they can be overridden with explicit flags.`,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			destroyOpts.Name = args[0]
			destroyOpts.Namespace = hubOpts.Namespace
			destroyOpts.Log = log.Log
			return runDeleteAWS(cmd, hubOpts, destroyOpts, args[0])
		},
	}

	cmd.Flags().StringVar(&destroyOpts.InfraID, "infra-id", "",
		"Infrastructure ID to destroy (inferred from the HostedCluster by default)")
	cmd.Flags().StringVar(&destroyOpts.AWSPlatform.Region, "region", destroyOpts.AWSPlatform.Region,
		"AWS region (inferred from the HostedCluster by default)")
	cmd.Flags().StringVar(&destroyOpts.AWSPlatform.BaseDomain, "base-domain", "",
		"Base domain (inferred from the HostedCluster by default)")
	cmd.Flags().BoolVar(&destroyOpts.AWSPlatform.PreserveIAM, "preserve-iam", false,
		"If true, skip deleting IAM roles and OIDC provider")
	destroyOpts.AWSPlatform.Credentials.BindProductFlags(cmd.Flags())

	return cmd
}

func runDeleteAWS(cmd *cobra.Command, hubOpts *Options, destroyOpts *core.DestroyOptions, name string) error {
	if hubOpts.HostingCluster == "" {
		return fmt.Errorf("--hosting-cluster is required")
	}

	if err := destroyOpts.AWSPlatform.Credentials.Validate(); err != nil {
		return err
	}

	// Step 1: GET the HC from the proxy to infer infra-id, region, base-domain.
	hc, err := getHostedClusterFromProxy(hubOpts, name)
	if err != nil {
		destroyOpts.Log.Info("Could not fetch HostedCluster from proxy (may already be deleted), using explicit flags", "error", err.Error())
	} else if hc != nil {
		if !cmd.Flags().Changed("infra-id") && hc.Spec.InfraID != "" {
			destroyOpts.InfraID = hc.Spec.InfraID
		}
		if !cmd.Flags().Changed("region") && hc.Spec.Platform.AWS != nil && hc.Spec.Platform.AWS.Region != "" {
			destroyOpts.AWSPlatform.Region = hc.Spec.Platform.AWS.Region
		}
		if !cmd.Flags().Changed("base-domain") && hc.Spec.DNS.BaseDomain != "" {
			destroyOpts.AWSPlatform.BaseDomain = hc.Spec.DNS.BaseDomain
		}
		if hc.Spec.DNS.BaseDomainPrefix != nil {
			if *hc.Spec.DNS.BaseDomainPrefix == "" {
				destroyOpts.AWSPlatform.BaseDomainPrefix = "none"
			} else {
				destroyOpts.AWSPlatform.BaseDomainPrefix = *hc.Spec.DNS.BaseDomainPrefix
			}
		}
	}

	// Step 2: Destroy AWS infra and IAM first.
	// If this fails the HostedCluster is still intact on the spoke, so the user
	// can retry with all parameters still available. Only proceed to HC deletion
	// once infra teardown succeeds.
	if err := destroyAWSInfra(cmd.Context(), destroyOpts); err != nil {
		return err
	}

	// Step 3: DELETE the HostedCluster via the HCP proxy.
	if err := runDelete(cmd, hubOpts, name); err != nil {
		return err
	}

	// Step 4: Force-remove any lingering HyperShift finalizer.
	// After infra is gone and the proxy DELETE is sent, the HyperShift controller
	// on the hosting cluster may get stuck trying to reconcile credentials into a
	// namespace that is already terminating. Since the infra is already destroyed
	// there is nothing left for the finalizer to protect, so we remove it so the
	// HC object is garbage-collected without manual intervention.
	return removeHCFinalizer(cmd.Context(), hubOpts, name)
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
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	fmt.Printf("HostedCluster %q deleted from %s\n", name, opts.HostingCluster)
	return nil
}

// getHostedClusterFromProxy fetches a HostedCluster via the HCP proxy GET endpoint.
// Returns (nil, nil) if the cluster is not found.
func getHostedClusterFromProxy(opts *Options, name string) (*hypershiftv1beta1.HostedCluster, error) {
	httpClient, host, err := buildHTTPClient(opts)
	if err != nil {
		return nil, err
	}

	url := hostedClusterURL(host, opts.Namespace, name, opts.HostingCluster)
	req, err := newRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("proxy returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var bundle struct {
		HostedCluster *hypershiftv1beta1.HostedCluster `json:"hostedCluster"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		return nil, fmt.Errorf("decode GET response: %w", err)
	}
	return bundle.HostedCluster, nil
}

// removeHCFinalizer patches the HostedCluster on the hosting cluster (via the
// hub kubeconfig) to remove any remaining finalizers so the object is
// garbage-collected after infra teardown.
func removeHCFinalizer(ctx context.Context, opts *Options, name string) error {
	c, err := util.GetClient()
	if err != nil {
		// Non-fatal: log and continue — HC will eventually be collected or can be
		// cleaned up manually.
		log.Log.Info("Could not build kube client to remove finalizer (non-fatal)", "error", err.Error())
		return nil
	}

	hc := &hypershiftv1beta1.HostedCluster{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: opts.Namespace, Name: name}, hc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // already gone
		}
		log.Log.Info("Could not fetch HostedCluster to remove finalizer (non-fatal)", "error", err.Error())
		return nil
	}

	if len(hc.Finalizers) == 0 {
		return nil
	}

	patch := client.MergeFrom(hc.DeepCopy())
	hc.Finalizers = nil
	if err := c.Patch(ctx, hc, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		log.Log.Info("Could not remove finalizer from HostedCluster (non-fatal)", "error", err.Error())
	}
	return nil
}

// destroyAWSInfra destroys AWS VPC infrastructure and IAM resources for the
// given DestroyOptions. It calls the infra and IAM destroy functions directly,
// bypassing core.DestroyCluster so that teardown is not blocked by HostedCluster
// finalizer processing.
func destroyAWSInfra(ctx context.Context, o *core.DestroyOptions) error {
	var errs []error

	o.Log.Info("Destroying infrastructure", "infraID", o.InfraID)
	destroyInfraOpts := awsinfra.DestroyInfraOptions{
		Region:              o.AWSPlatform.Region,
		InfraID:             o.InfraID,
		AWSCredentialsOpts:  &awsinfra.DelegatedAWSCredentialOptions{AWSCredentialsOpts: &o.AWSPlatform.Credentials},
		Name:                o.Name,
		BaseDomain:          o.AWSPlatform.BaseDomain,
		BaseDomainPrefix:    o.AWSPlatform.BaseDomainPrefix,
		AwsInfraGracePeriod: o.AWSPlatform.AwsInfraGracePeriod,
		Log:                 o.Log,
	}
	if err := destroyInfraOpts.Run(ctx); err != nil {
		errs = append(errs, fmt.Errorf("failed to destroy infrastructure: %w", err))
	}

	if !o.AWSPlatform.PreserveIAM {
		o.Log.Info("Destroying IAM", "infraID", o.InfraID)
		destroyIAMOpts := awsinfra.DestroyIAMOptions{
			Region:             o.AWSPlatform.Region,
			AWSCredentialsOpts: o.AWSPlatform.Credentials,
			InfraID:            o.InfraID,
			Log:                o.Log,
		}
		if err := destroyIAMOpts.Run(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to destroy IAM: %w", err))
		}
	}

	if err := utilerrors.NewAggregate(errs); err != nil {
		return err
	}
	o.Log.Info("Successfully destroyed cluster and infrastructure", "name", o.Name, "infraID", o.InfraID)
	return nil
}
