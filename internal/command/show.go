// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/terraform/internal/backend"
	"github.com/hashicorp/terraform/internal/command/arguments"
	"github.com/hashicorp/terraform/internal/command/jsonformat"
	"github.com/hashicorp/terraform/internal/command/views"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/plans/planfile"
	"github.com/hashicorp/terraform/internal/states/statefile"
	"github.com/hashicorp/terraform/internal/states/statemgr"
	"github.com/hashicorp/terraform/internal/terraform"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// ShowCommand is a Command implementation that reads and outputs the
// contents of a Terraform plan or state file.
type ShowCommand struct {
	Meta
}

func (c *ShowCommand) Run(rawArgs []string) int {
	// Parse and apply global view arguments
	common, rawArgs := arguments.ParseView(rawArgs)
	c.View.Configure(common)

	// Parse and validate flags
	args, diags := arguments.ParseShow(rawArgs)
	if diags.HasErrors() {
		c.View.Diagnostics(diags)
		c.View.HelpPrompt("show")
		return 1
	}

	// Set up view
	view := views.NewShow(args.ViewType, c.View)

	// Check for user-supplied plugin path
	var err error
	if c.pluginPath, err = c.loadPluginPath(); err != nil {
		diags = diags.Append(fmt.Errorf("error loading plugin path: %s", err))
		view.Diagnostics(diags)
		return 1
	}

	// Get the data we need to display
	plan, jsonPlan, stateFile, config, schemas, showDiags := c.show(args.Path)
	diags = diags.Append(showDiags)
	if showDiags.HasErrors() {
		view.Diagnostics(diags)
		return 1
	}

	// Display the data
	return view.Display(config, plan, jsonPlan, stateFile, schemas)
}

func (c *ShowCommand) Help() string {
	helpText := `
Usage: terraform [global options] show [options] [path]

  Reads and outputs a Terraform state or plan file in a human-readable
  form. If no path is specified, the current state will be shown.

Options:

  -no-color           If specified, output won't contain any color.
  -json               If specified, output the Terraform plan or state in
                      a machine-readable form.

`
	return strings.TrimSpace(helpText)
}

func (c *ShowCommand) Synopsis() string {
	return "Show the current state or a saved plan"
}

func (c *ShowCommand) show(path string) (*plans.Plan, *jsonformat.Plan, *statefile.File, *configs.Config, *terraform.Schemas, tfdiags.Diagnostics) {
	var diags, showDiags tfdiags.Diagnostics
	var plan *plans.Plan
	var jsonPlan *jsonformat.Plan
	var stateFile *statefile.File
	var config *configs.Config
	var schemas *terraform.Schemas

	// No plan file or state file argument provided,
	// so get the latest state snapshot
	if path == "" {
		stateFile, showDiags = c.showFromLatestStateSnapshot()
		diags = diags.Append(showDiags)
		if showDiags.HasErrors() {
			return plan, jsonPlan, stateFile, config, schemas, diags
		}
	}

	// Plan file or state file argument provided,
	// so try to load the argument as a plan file first.
	// If that fails, try to load it as a statefile.
	if path != "" {
		plan, jsonPlan, stateFile, config, showDiags = c.showFromPath(path)
		diags = diags.Append(showDiags)
		if showDiags.HasErrors() {
			return plan, jsonPlan, stateFile, config, schemas, diags
		}
	}

	// Get schemas, if possible
	if config != nil || stateFile != nil {
		schemas, diags = c.MaybeGetSchemas(stateFile.State, config)
		if diags.HasErrors() {
			return plan, jsonPlan, stateFile, config, schemas, diags
		}
	}

	return plan, jsonPlan, stateFile, config, schemas, diags
}
func (c *ShowCommand) showFromLatestStateSnapshot() (*statefile.File, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// Load the backend
	b, backendDiags := c.Backend(nil)
	diags = diags.Append(backendDiags)
	if backendDiags.HasErrors() {
		return nil, diags
	}
	c.ignoreRemoteVersionConflict(b)

	// Load the workspace
	workspace, err := c.Workspace()
	if err != nil {
		diags = diags.Append(fmt.Errorf("error selecting workspace: %s", err))
		return nil, diags
	}

	// Get the latest state snapshot from the backend for the current workspace
	stateFile, stateErr := getStateFromBackend(b, workspace)
	if stateErr != nil {
		diags = diags.Append(stateErr)
		return nil, diags
	}

	return stateFile, diags
}

func (c *ShowCommand) showFromPath(path string) (*plans.Plan, *jsonformat.Plan, *statefile.File, *configs.Config, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	var planErr, stateErr error
	var plan *plans.Plan
	var jsonPlan *jsonformat.Plan
	var stateFile *statefile.File
	var config *configs.Config

	// Path might be a local plan file, a bookmark to a saved cloud plan, or a
	// state file. First, try to get a plan and associated data from a local
	// plan file. If that fails, try to get a json plan from the path argument.
	// If that fails, try to get the statefile from the path argument.
	plan, jsonPlan, stateFile, config, planErr = getPlanFromPath(path)
	if planErr != nil {
		stateFile, stateErr = getStateFromPath(path)
		if stateErr != nil {
			diags = diags.Append(
				tfdiags.Sourceless(
					tfdiags.Error,
					"Failed to read the given file as a state or plan file",
					fmt.Sprintf("State read error: %s\n\nPlan read error: %s", stateErr, planErr),
				),
			)
			return nil, nil, nil, nil, diags
		}
	}
	return plan, jsonPlan, stateFile, config, diags
}

// getPlanFromPath returns a plan, json plan, statefile, and config if the
// user-supplied path points to either a local or cloud plan file. Note that
// some of the return values will be nil no matter what; local plan files do not
// yield a json plan, and cloud plans do not yield real plan/state/config
// structs. An error generally suggests that the given path is either a
// directory or a statefile.
func getPlanFromPath(path string) (*plans.Plan, *jsonformat.Plan, *statefile.File, *configs.Config, error) {
	var err error
	var plan *plans.Plan
	var jsonPlan *jsonformat.Plan
	var stateFile *statefile.File
	var config *configs.Config

	pf, err := planfile.OpenWrapped(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if pf.IsLocal() {
		plan, stateFile, config, err = getDataFromPlanfileReader(pf.Local)
	}

	// TODO: get jsonplan from cloud pf

	return plan, jsonPlan, stateFile, config, err
}

// getDataFromPlanfileReader returns a plan, statefile, and config, extracted from a local plan file.
func getDataFromPlanfileReader(planReader *planfile.Reader) (*plans.Plan, *statefile.File, *configs.Config, error) {
	// Get plan
	plan, err := planReader.ReadPlan()
	if err != nil {
		return nil, nil, nil, err
	}

	// Get statefile
	stateFile, err := planReader.ReadStateFile()
	if err != nil {
		return nil, nil, nil, err
	}

	// Get config
	config, diags := planReader.ReadConfig()
	if diags.HasErrors() {
		return nil, nil, nil, diags.Err()
	}

	return plan, stateFile, config, err
}

// getStateFromPath returns a statefile if the user-supplied path points to a statefile.
func getStateFromPath(path string) (*statefile.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Error loading statefile: %s", err)
	}
	defer file.Close()

	var stateFile *statefile.File
	stateFile, err = statefile.Read(file)
	if err != nil {
		return nil, fmt.Errorf("Error reading %s as a statefile: %s", path, err)
	}
	return stateFile, nil
}

// getStateFromBackend returns the State for the current workspace, if available.
func getStateFromBackend(b backend.Backend, workspace string) (*statefile.File, error) {
	// Get the state store for the given workspace
	stateStore, err := b.StateMgr(workspace)
	if err != nil {
		return nil, fmt.Errorf("Failed to load state manager: %s", err)
	}

	// Refresh the state store with the latest state snapshot from persistent storage
	if err := stateStore.RefreshState(); err != nil {
		return nil, fmt.Errorf("Failed to load state: %s", err)
	}

	// Get the latest state snapshot and return it
	stateFile := statemgr.Export(stateStore)
	return stateFile, nil
}
