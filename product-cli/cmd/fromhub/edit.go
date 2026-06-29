package fromhub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

func newEditCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "edit <name>",
		Short:        "Edit a HostedCluster in your $EDITOR (like kubectl edit)",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEdit(cmd, opts, args[0])
		},
	}
	return cmd
}

func runEdit(cmd *cobra.Command, opts *Options, name string) error {
	if opts.HostingCluster == "" {
		return fmt.Errorf("--hosting-cluster is required")
	}

	httpClient, host, err := buildHTTPClient(opts)
	if err != nil {
		return err
	}

	// --- 1. Fetch current state ---
	getURL := hostedClusterURL(host, opts.Namespace, name, opts.HostingCluster)
	resp, err := httpClient.Get(getURL)
	if err != nil {
		return fmt.Errorf("GET HostedCluster: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var hc hypershiftv1beta1.HostedCluster
	if err := json.NewDecoder(resp.Body).Decode(&hc); err != nil {
		return fmt.Errorf("decode HostedCluster: %w", err)
	}

	// Keep a canonical JSON snapshot of the original for merge-patch diffing.
	originalJSON, err := json.Marshal(hc)
	if err != nil {
		return fmt.Errorf("marshal original: %w", err)
	}

	// --- 2. Serialise to YAML with an explanatory header ---
	resourceYAML, err := yaml.Marshal(hc)
	if err != nil {
		return fmt.Errorf("marshal to YAML: %w", err)
	}
	header := fmt.Sprintf(
		"# Edit the object below. Lines beginning with '#' are ignored.\n"+
			"# Saving without changes exits without updating.\n"+
			"# HostedCluster %s/%s on hosting cluster %q\n\n",
		hc.Namespace, hc.Name, opts.HostingCluster,
	)

	// --- 3. Edit loop (retry on invalid YAML or server rejection) ---
	content := []byte(header + string(resourceYAML))
	var serverErrMsg string

	for {
		// Prepend any previous server error so the user can see why they're back.
		editContent := content
		if serverErrMsg != "" {
			editContent = []byte("# ERROR applying last save: " + serverErrMsg + "\n" + string(content))
			serverErrMsg = ""
		}

		edited, changed, err := openInEditor(editContent)
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintln(cmd.OutOrStdout(), "Edit cancelled, no changes made.")
			return nil
		}

		// Strip comment lines before parsing
		cleaned := stripComments(edited)

		// Validate YAML and convert to JSON
		editedJSON, err := yaml.YAMLToJSON(cleaned)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"error: the edited file failed YAML validation: %v\n"+
					"A copy of your changes is in the editor; fix the error and save again.\n", err)
			// Keep the user's edits (cleaned of comments) as the new content to re-edit.
			content = edited
			continue
		}

		// Compute a strategic merge patch (respects Kubernetes merge keys).
		patch, err := strategicpatch.CreateTwoWayMergePatch(originalJSON, editedJSON, hypershiftv1beta1.HostedCluster{})
		if err != nil {
			return fmt.Errorf("compute merge patch: %w", err)
		}
		if string(patch) == "{}" {
			fmt.Fprintln(cmd.OutOrStdout(), "No effective changes after parsing.")
			return nil
		}

		// --- 4. PATCH through the proxy ---
		patchURL := hostedClusterURL(host, opts.Namespace, name, opts.HostingCluster)
		req, err := newRequest(http.MethodPatch, patchURL, bytes.NewReader(patch))
		if err != nil {
			return fmt.Errorf("build PATCH request: %w", err)
		}
		req.Header.Set("Content-Type", "application/merge-patch+json")

		patchResp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("PATCH request: %w", err)
		}
		defer patchResp.Body.Close()

		if patchResp.StatusCode == http.StatusOK {
			fmt.Fprintf(cmd.OutOrStdout(), "hostedcluster/%s edited\n", name)
			return nil
		}

		// Server rejected — show error, let user fix and re-save.
		errBody, _ := io.ReadAll(patchResp.Body)
		serverErrMsg = fmt.Sprintf("server returned %d: %s", patchResp.StatusCode, strings.TrimSpace(string(errBody)))
		content = edited
	}
}

// openInEditor writes content to a temp file, opens it in the user's preferred
// editor ($VISUAL → $EDITOR → platform default), and returns the (possibly
// modified) file content plus whether it changed.
func openInEditor(content []byte) (edited []byte, changed bool, err error) {
	tmp, err := os.CreateTemp("", "hcp-edit-*.yaml")
	if err != nil {
		return nil, false, fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(content); err != nil {
		return nil, false, fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, false, fmt.Errorf("close temp file: %w", err)
	}

	editor := resolveEditor()
	// Some editors (e.g. "code --wait") embed spaces in the command.
	parts := strings.Fields(editor)
	parts = append(parts, tmp.Name())

	editorCmd := exec.Command(parts[0], parts[1:]...) //nolint:gosec
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return nil, false, fmt.Errorf("editor %q exited with error: %w", editor, err)
	}

	edited, err = os.ReadFile(tmp.Name())
	if err != nil {
		return nil, false, fmt.Errorf("read edited file: %w", err)
	}

	return edited, !bytes.Equal(content, edited), nil
}

// resolveEditor returns the editor to use: $VISUAL → $EDITOR → platform default.
func resolveEditor() string {
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	if v := os.Getenv("EDITOR"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		return "notepad"
	}
	return "vi"
}

// stripComments removes lines that begin with '#' (after trimming leading spaces).
func stripComments(b []byte) []byte {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			out = append(out, line)
		}
	}
	return []byte(strings.Join(out, "\n"))
}
