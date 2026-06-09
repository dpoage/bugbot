package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
)

// newInitCmd writes a commented starter config to the current directory. It
// refuses to overwrite an existing file.
func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter bugbot.yaml to the current directory",
		Long: `init writes a commented starter ` + config.DefaultFileName + ` to the current
directory. It refuses to overwrite an existing file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := config.DefaultFileName

			// Refuse to clobber an existing file. Use O_EXCL so the
			// check-and-create is atomic.
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				if errors.Is(err, fs.ErrExist) {
					return fmt.Errorf("%s already exists; refusing to overwrite", path)
				}
				return fmt.Errorf("create %s: %w", path, err)
			}
			defer f.Close()

			if _, err := f.WriteString(config.StarterYAML); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
			fmt.Fprintln(cmd.OutOrStdout(), "Next: set the api_key_env variables, then run `bugbot scan`.")
			return nil
		},
	}
	return cmd
}
