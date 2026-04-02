package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/version"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

var (
	// commitFromGit is a constant representing the source version that
	// generated this build. It should be set during build via -ldflags.
	commitFromGit string
	// versionFromGit is a constant representing the version tag that
	// generated this build. It should be set during build via -ldflags.
	versionFromGit = "unknown"
	// major version
	majorFromGit string
	// minor version
	minorFromGit string
	// patch version
	patchFromGit string
	// build date in ISO8601 format, output of $(date -u +'%Y-%m-%dT%H:%M:%SZ')
	buildDate string
	// state of git tree, either "clean" or "dirty"
	gitTreeState string
	// kine version information structure
	KineVersionInfo Info
)

type Info struct {
	version.Info
	Patch string `json:"patch"`
}

type VersionOptions struct {
	Output string

	genericclioptions.IOStreams
}

func NewVersionOptions(ioStreams genericclioptions.IOStreams) *VersionOptions {
	return &VersionOptions{
		IOStreams: ioStreams,
	}
}

func NewVersionCommand(ioStreams genericclioptions.IOStreams) *cobra.Command {
	KineVersionInfo = Info{
		Info: version.Info{
			Major:        majorFromGit,
			Minor:        minorFromGit,
			GitCommit:    commitFromGit,
			GitVersion:   versionFromGit,
			GitTreeState: gitTreeState,
			BuildDate:    buildDate,
			GoVersion:    runtime.Version(),
			Compiler:     runtime.Compiler,
			Platform:     fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		},
		Patch: patchFromGit,
	}

	o := NewVersionOptions(ioStreams)
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print MicroShift-kine version information",
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Run())
		},
	}

	cmd.Flags().StringVarP(&o.Output, "output", "o", o.Output, "One of 'yaml' or 'json'.")

	return cmd
}

func (o *VersionOptions) Run() error {
	switch o.Output {
	case "":
		fmt.Fprintf(o.Out, "MicroShift-kine Version: %s\n", KineVersionInfo.String())
	case "yaml":
		marshalled, err := yaml.Marshal(&KineVersionInfo)
		if err != nil {
			return err
		}
		fmt.Fprintln(o.Out, string(marshalled))
	case "json":
		marshalled, err := json.MarshalIndent(&KineVersionInfo, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(o.Out, string(marshalled))
	default:
		return fmt.Errorf("VersionOptions were not validated: --output=%q should have been rejected", o.Output)
	}

	return nil
}
