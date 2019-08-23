package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/logicmonitor/k8s-release-manager/pkg/config"
	"github.com/logicmonitor/k8s-release-manager/pkg/export"
	"github.com/logicmonitor/k8s-release-manager/pkg/state"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var daemon bool
var mgrstate *state.State
var pollingInterval int
var namespaces string

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export Helm release state",
	Long: `
Release Manager Export will contact Tiller in the configured cluster, collect
all metadata for each deployed release, and write that metadata to the
configured backend. This metadata can later be consumed by Release Manager
import to re-install the saved releases to a different cluster.

Export can also be run in daemon mode to continuously update the stored state to
reflect ongoing changes to the cluster.

Installing releasemanager daemon via Helm chart
	helm repo add logicmonitor https://logicmonitor.github.io/k8s-helm-charts
	helm install logicmonitor/releasemanager \
    --set path=$BACKEND_STORAGE_PATH \
  --name releasemanager-$CURRENT_CLUSTER

When running in daemon mode, it is HIGHLY recommended to use the
official Release Manager Helm chart. Failing to specify --release-name or
use the official Helm chart can lead to multiple Release Manager instances
writing state to the same backend path, causing conflicts, overwrites, chaos.`,
	PreRun: func(cmd *cobra.Command, args []string) {
		valid := validateCommonConfig()
		if !valid {
			failAuth(cmd)
		}

		ns, err := parseNamespaces(viper.GetString("namespaces"))
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		rlsmgrconfig.Export = &config.ExportConfig{
			DaemonMode:      viper.GetBool("daemon"),
			ReleaseName:     viper.GetString("releaseName"),
			PollingInterval: viper.GetInt64("pollingInterval"),
			Namespaces: 		 ns,
		}
	},
}

func init() { // nolint: dupl
	exportCmd.PersistentFlags().BoolVarP(&daemon, "daemon", "", false, "Run in daemon mode and periodically export the current state")
	exportCmd.PersistentFlags().IntVarP(&pollingInterval, "polling-interval", "p", 30, "Specify, in seconds, how frequently the daemon should export the current state")
	exportCmd.PersistentFlags().StringVarP(&releaseName, "release-name", "", "", "Specify the Release Manager daemon's Helm release name")
	exportCmd.PersistentFlags().StringVarP(&namespaces, "namespaces", "", "", "A comma-delimited list of namespaces to export. The default behavior is to export all namespaces")
	err := bindConfigFlags(exportCmd, map[string]string{
		"daemon":          "daemon",
		"pollingInterval": "polling-interval",
		"releaseName":     "release-name",
		"namespaces":			 "namespaces",
	})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	RootCmd.AddCommand(exportCmd)
}

func exportRun(cmd *cobra.Command, args []string) { // nolint: dupl
	// Instantiate the Release Manager.
	export, err := export.New(rlsmgrconfig, mgrstate)
	if err != nil {
		log.Fatalf("Failed to create Release Manager exporter: %v", err)
	}

	err = export.Run()
	if err != nil {
		log.Errorf("%v", err)
	}
}

func parseNamespaces(namespaces string) (map[string]string, error) {
	results := make(map[string]string)
	// remove whitespace and split
	ns := strings.Split(strings.Replace(namespaces, " ", "", -1), ",")
	for _, n := range ns {
		results[n] = n
	}
	return  results, nil
}
