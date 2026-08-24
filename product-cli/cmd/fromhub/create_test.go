package fromhub

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// bindTestFlags registers the same flag names/types core.BindOptions binds
// for the flags exercised by unsupportedFlags, plus one unrelated flag
// ("name") to verify unrelated flags are left untouched.
func bindTestFlags(flags *pflag.FlagSet) {
	flags.Bool("version-check", false, "")
	flags.Bool("wait", false, "")
	flags.Duration("timeout", 0, "")
	flags.Bool("render", false, "")
	flags.String("render-into", "", "")
	flags.Bool("render-sensitive", false, "")
	flags.String("name", "", "")
}

func TestHideFlags(t *testing.T) {
	t.Run("When hideFlags runs, it should hide every listed unsupported flag", func(t *testing.T) {
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		bindTestFlags(flags)

		hideFlags(flags, unsupportedFlags)

		for _, f := range unsupportedFlags {
			flag := flags.Lookup(f.name)
			require.NotNil(t, flag, "flag %q should exist", f.name)
			require.True(t, flag.Hidden, "flag %q should be hidden", f.name)
		}
	})

	t.Run("When hideFlags runs, it should leave unrelated flags visible", func(t *testing.T) {
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		bindTestFlags(flags)

		hideFlags(flags, unsupportedFlags)

		flag := flags.Lookup("name")
		require.NotNil(t, flag)
		require.False(t, flag.Hidden, "unrelated flag %q should stay visible", "name")
	})
}

func TestRejectFlags(t *testing.T) {
	t.Run("When no unsupported flags were set, it should return nil", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		bindTestFlags(cmd.Flags())

		require.NoError(t, rejectFlags(cmd, unsupportedFlags))
	})

	t.Run("When an unrelated flag was set, it should return nil", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		bindTestFlags(cmd.Flags())
		require.NoError(t, cmd.Flags().Set("name", "my-cluster"))

		require.NoError(t, rejectFlags(cmd, unsupportedFlags))
	})

	t.Run("When an unsupported flag isn't registered on the command at all, it should return nil", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		// secret-creds is AWS-only; a command that never binds it should never
		// be rejected because of it, since pflag.Changed returns false for
		// unknown flag names instead of panicking.
		require.NoError(t, rejectFlags(cmd, awsUnsupportedFlags))
	})

	for _, f := range unsupportedFlags {
		f := f
		t.Run("When --"+f.name+" is explicitly set, it should return an error naming the flag", func(t *testing.T) {
			cmd := &cobra.Command{Use: "test"}
			bindTestFlags(cmd.Flags())

			var value string
			switch f.name {
			case "timeout":
				value = time.Second.String()
			case "render-into":
				value = "/tmp/out.yaml"
			default:
				value = "true"
			}
			require.NoError(t, cmd.Flags().Set(f.name, value))

			err := rejectFlags(cmd, unsupportedFlags)
			require.Error(t, err)
			require.Contains(t, err.Error(), "--"+f.name)
		})
	}
}

func TestAWSUnsupportedFlags(t *testing.T) {
	t.Run("When --secret-creds is not set, it should return nil", func(t *testing.T) {
		cmd := &cobra.Command{Use: "aws"}
		cmd.Flags().String("secret-creds", "", "")

		require.NoError(t, rejectFlags(cmd, awsUnsupportedFlags))
	})

	t.Run("When --secret-creds is explicitly set, it should return an error naming the flag", func(t *testing.T) {
		cmd := &cobra.Command{Use: "aws"}
		cmd.Flags().String("secret-creds", "", "")
		require.NoError(t, cmd.Flags().Set("secret-creds", "my-creds-secret"))

		err := rejectFlags(cmd, awsUnsupportedFlags)
		require.Error(t, err)
		require.Contains(t, err.Error(), "--secret-creds")
	})

	t.Run("When hideFlags runs on an AWS flag set, it should hide --secret-creds", func(t *testing.T) {
		flags := pflag.NewFlagSet("aws", pflag.ContinueOnError)
		flags.String("secret-creds", "", "")

		hideFlags(flags, awsUnsupportedFlags)

		flag := flags.Lookup("secret-creds")
		require.NotNil(t, flag)
		require.True(t, flag.Hidden)
	})
}
