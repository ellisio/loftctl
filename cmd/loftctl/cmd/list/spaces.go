package list

import (
	"github.com/loft-sh/loftctl/cmd/loftctl/flags"
	"github.com/loft-sh/loftctl/pkg/client"
	"github.com/loft-sh/loftctl/pkg/client/helper"
	"github.com/loft-sh/loftctl/pkg/log"
	"github.com/loft-sh/loftctl/pkg/upgrade"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/duration"
	"time"
)

// SpacesCmd holds the login cmd flags
type SpacesCmd struct {
	*flags.GlobalFlags

	log log.Logger
}

// NewSpacesCmd creates a new spaces command
func NewSpacesCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &SpacesCmd{
		GlobalFlags: globalFlags,
		log:         log.GetInstance(),
	}
	description := `
#######################################################
################## loft list spaces ###################
#######################################################
List the loft spaces you have access to

Example:
loft list spaces
#######################################################
	`
	if upgrade.IsPlugin == "true" {
		description = `
#######################################################
################ devspace list spaces #################
#######################################################
List the loft spaces you have access to

Example:
devspace list spaces
#######################################################
	`
	}
	loginCmd := &cobra.Command{
		Use:   "spaces",
		Short: "Lists the loft spaces you have access to",
		Long:  description,
		Args:  cobra.NoArgs,
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return cmd.RunSpaces(cobraCmd, args)
		},
	}

	return loginCmd
}

// RunUsers executes the functionality "loft list users"
func (cmd *SpacesCmd) RunSpaces(cobraCmd *cobra.Command, args []string) error {
	baseClient, err := client.NewClientFromPath(cmd.Config)
	if err != nil {
		return err
	}

	spaces, err := helper.GetSpaces(baseClient)
	if err != nil {
		return err
	}

	header := []string{
		"Name",
		"Cluster",
		"Sleeping",
		"Status",
		"Age",
	}
	values := [][]string{}
	for _, space := range spaces {
		sleepModeConfig := space.SleepModeConfig
		sleeping := "false"
		if sleepModeConfig.Status.SleepingSince != 0 {
			sleeping = duration.HumanDuration(time.Now().Sub(time.Unix(sleepModeConfig.Status.SleepingSince, 0)))
		}

		values = append(values, []string{
			space.Space.Name,
			space.Cluster,
			sleeping,
			string(space.Space.Status.Phase),
			duration.HumanDuration(time.Now().Sub(space.Space.CreationTimestamp.Time)),
		})
	}

	log.PrintTable(cmd.log, header, values)
	return nil
}
