package cmd

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/util/wait"
	"time"

	"github.com/loft-sh/loftctl/cmd/loftctl/flags"
	"github.com/loft-sh/loftctl/pkg/client"
	"github.com/loft-sh/loftctl/pkg/client/helper"
	"github.com/loft-sh/loftctl/pkg/log"
	"github.com/loft-sh/loftctl/pkg/upgrade"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WakeUpCmd holds the cmd flags
type WakeUpCmd struct {
	*flags.GlobalFlags

	Cluster string
	Log     log.Logger
}

// NewWakeUpCmd creates a new command
func NewWakeUpCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &WakeUpCmd{
		GlobalFlags: globalFlags,
		Log:         log.GetInstance(),
	}

	description := `
#######################################################
###################### loft wakeup ####################
#######################################################
wakeup resumes a sleeping space

Example:
loft wakeup myspace
loft wakeup myspace --cluster mycluster
#######################################################
	`
	if upgrade.IsPlugin == "true" {
		description = `
#######################################################
################### devspace wakeup ###################
#######################################################
wakeup resumes a sleeping space

Example:
devspace wakeup myspace
devspace wakeup myspace --cluster mycluster
#######################################################
	`
	}

	c := &cobra.Command{
		Use:   "wakeup",
		Short: "Wakes up a space",
		Long:  description,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return cmd.Run(cobraCmd, args)
		},
	}

	c.Flags().StringVar(&cmd.Cluster, "cluster", "", "The cluster to use")
	return c
}

// Run executes the functionality
func (cmd *WakeUpCmd) Run(cobraCmd *cobra.Command, args []string) error {
	baseClient, err := client.NewClientFromPath(cmd.Config)
	if err != nil {
		return err
	}

	spaceName := ""
	if len(args) > 0 {
		spaceName = args[0]
	}

	spaceName, clusterName, err := helper.SelectSpaceAndClusterName(baseClient, spaceName, cmd.Cluster, cmd.Log)
	if err != nil {
		return err
	}

	clusterClient, err := baseClient.Cluster(clusterName)
	if err != nil {
		return err
	}

	configs, err := clusterClient.Agent().ClusterV1().SleepModeConfigs(spaceName).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	sleepModeConfig := &configs.Items[0]
	sleepModeConfig.Spec.ForceSleep = false
	sleepModeConfig.Spec.ForceSleepDuration = nil
	sleepModeConfig.Status.LastActivity = time.Now().Unix()

	sleepModeConfig, err = clusterClient.Agent().ClusterV1().SleepModeConfigs(spaceName).Create(context.TODO(), sleepModeConfig, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// wait for sleeping
	cmd.Log.StartWait("Wait until space wakes up")
	defer cmd.Log.StopWait()
	err = wait.Poll(time.Second, time.Minute, func() (bool, error) {
		configs, err := clusterClient.Agent().ClusterV1().SleepModeConfigs(spaceName).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return false, err
		}

		return configs.Items[0].Status.SleepingSince == 0, nil
	})
	if err != nil {
		return fmt.Errorf("error waiting for space to wake up: %v", err)
	}

	cmd.Log.Donef("Successfully woken up space %s", spaceName)
	return nil
}
