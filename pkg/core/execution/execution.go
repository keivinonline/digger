package execution

import (
	"digger/pkg/core/locking"
	"digger/pkg/core/models"
	"digger/pkg/core/reporting"
	"digger/pkg/core/runners"
	"digger/pkg/core/storage"
	"digger/pkg/core/terraform"
	"digger/pkg/core/utils"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"strings"
)

type Executor interface {
	Plan() (bool, string, error)
	Apply() (bool, error)
}

type LockingExecutorWrapper struct {
	ProjectLock locking.ProjectLock
	Executor    Executor
}

func (l LockingExecutorWrapper) Plan() (bool, string, error) {
	plan := ""
	locked, err := l.ProjectLock.Lock()
	if err != nil {
		return false, "", fmt.Errorf("error locking project: %v", err)
	}
	log.Printf("Lock result: %t\n", locked)
	if locked {
		return l.Executor.Plan()
	} else {
		return false, plan, nil
	}
}

func (l LockingExecutorWrapper) Apply() (bool, error) {
	locked, err := l.ProjectLock.Lock()
	if err != nil {
		return false, fmt.Errorf("error locking project: %v", err)
	}
	log.Printf("Lock result: %t\n", locked)
	if locked {
		return l.Executor.Apply()
	} else {
		return false, nil
	}
}

func (l LockingExecutorWrapper) Unlock() error {
	err := l.ProjectLock.ForceUnlock()
	if err != nil {
		return fmt.Errorf("failed to aquire lock: %s, %v", l.ProjectLock.LockId(), err)
	}
	return nil
}

func (l LockingExecutorWrapper) Lock() error {
	_, err := l.ProjectLock.Lock()
	if err != nil {
		return fmt.Errorf("failed to aquire lock: %s, %v", l.ProjectLock.LockId(), err)
	}
	return nil
}

type DiggerExecutor struct {
	ProjectNamespace  string
	ProjectName       string
	ProjectPath       string
	StateEnvVars      map[string]string
	CommandEnvVars    map[string]string
	ApplyStage        *models.Stage
	PlanStage         *models.Stage
	CommandRunner     runners.CommandRun
	TerraformExecutor terraform.TerraformExecutor
	Reporter          reporting.Reporter
	PlanStorage       storage.PlanStorage
	PlanPathProvider  PlanPathProvider
}

type PlanPathProvider interface {
	LocalPlanFilePath() string
	StoredPlanFilePath() string
	PlanFileName() string
}

type ProjectPathProvider struct {
	ProjectPath      string
	ProjectNamespace string
	ProjectName      string
}

func (d ProjectPathProvider) PlanFileName() string {
	return strings.ReplaceAll(d.ProjectNamespace, "/", ":") + "#" + d.ProjectName + ".tfplan"
}

func (d ProjectPathProvider) LocalPlanFilePath() string {
	return path.Join(d.ProjectPath, d.PlanFileName())
}

func (d ProjectPathProvider) StoredPlanFilePath() string {
	return path.Join(d.ProjectNamespace, d.PlanFileName())
}

func (d DiggerExecutor) Plan() (bool, string, error) {
	plan := ""
	var planSteps []models.Step

	if d.PlanStage != nil {
		planSteps = d.PlanStage.Steps
	} else {
		planSteps = []models.Step{
			{
				Action: "init",
			},
			{
				Action: "plan",
			},
		}
	}
	for _, step := range planSteps {
		if step.Action == "init" {
			_, _, err := d.TerraformExecutor.Init(step.ExtraArgs, d.StateEnvVars)
			if err != nil {
				return false, "", fmt.Errorf("error running init: %v", err)
			}
		}
		if step.Action == "plan" {
			planArgs := []string{"-out", d.PlanPathProvider.PlanFileName()}
			planArgs = append(planArgs, step.ExtraArgs...)
			isNonEmptyPlan, stdout, stderr, err := d.TerraformExecutor.Plan(planArgs, d.CommandEnvVars)
			if err != nil {
				return false, "", fmt.Errorf("error executing plan: %v", err)
			}
			if d.PlanStorage != nil {
				planExists, err := d.PlanStorage.PlanExists(d.PlanPathProvider.StoredPlanFilePath())
				if err != nil {
					return false, "", fmt.Errorf("error checking if plan exists: %v", err)
				}

				if planExists {
					err = d.PlanStorage.DeleteStoredPlan(d.PlanPathProvider.StoredPlanFilePath())
					if err != nil {
						return false, "", fmt.Errorf("error deleting plan: %v", err)
					}
				}

				err = d.PlanStorage.StorePlan(d.PlanPathProvider.LocalPlanFilePath(), d.PlanPathProvider.StoredPlanFilePath())
				if err != nil {
					return false, "", fmt.Errorf("error storing plan: %v", err)
				}
			}
			plan = cleanupTerraformPlan(isNonEmptyPlan, err, stdout, stderr)
			if err != nil {
				fmt.Printf("error publishing comment: %v", err)
			}
		}
		if step.Action == "run" {
			var commands []string
			if os.Getenv("ACTIVATE_VENV") == "true" {
				commands = append(commands, fmt.Sprintf("source %v/.venv/bin/activate", os.Getenv("GITHUB_WORKSPACE")))
			}
			commands = append(commands, step.Value)
			log.Printf("Running %v for **%v**\n", step.Value, d.ProjectNamespace+"#"+d.ProjectName)
			_, _, err := d.CommandRunner.Run(d.ProjectPath, step.Shell, commands)
			if err != nil {
				return false, "", fmt.Errorf("error running command: %v", err)
			}
		}
	}
	return true, plan, nil
}

func (d DiggerExecutor) Apply() (bool, error) {
	var plansFilename *string
	if d.PlanStorage != nil {
		var err error
		plansFilename, err = d.PlanStorage.RetrievePlan(d.PlanPathProvider.LocalPlanFilePath(), d.PlanPathProvider.StoredPlanFilePath())
		if err != nil {
			return false, fmt.Errorf("error retrieving plan: %v", err)
		}
	}

	var applySteps []models.Step

	if d.ApplyStage != nil {
		applySteps = d.ApplyStage.Steps
	} else {
		applySteps = []models.Step{
			{
				Action: "init",
			},
			{
				Action: "apply",
			},
		}
	}

	for _, step := range applySteps {
		if step.Action == "init" {
			_, _, err := d.TerraformExecutor.Init(step.ExtraArgs, d.StateEnvVars)
			if err != nil {
				return false, fmt.Errorf("error running init: %v", err)
			}
		}
		if step.Action == "apply" {
			stdout, stderr, err := d.TerraformExecutor.Apply(step.ExtraArgs, plansFilename, d.CommandEnvVars)
			applyOutput := cleanupTerraformApply(true, err, stdout, stderr)
			formatter := utils.GetTerraformOutputAsCollapsibleComment("Apply for <b>" + d.ProjectNamespace + "#" + d.ProjectName + "</b>")

			commentErr := d.Reporter.Report(applyOutput, formatter)
			if commentErr != nil {
				fmt.Printf("error publishing comment: %v", err)
			}
			if err != nil {
				commentErr = d.Reporter.Report(err.Error(), utils.AsCollapsibleComment("Error during applying."))
				if commentErr != nil {
					fmt.Printf("error publishing comment: %v", err)
				}
				return false, fmt.Errorf("error executing apply: %v", err)
			}
		}
		if step.Action == "run" {
			var commands []string
			if os.Getenv("ACTIVATE_VENV") == "true" {
				commands = append(commands, fmt.Sprintf("source %v/.venv/bin/activate", d.ProjectPath))
			}
			commands = append(commands, step.Value)
			log.Printf("Running %v for **%v**\n", step.Value, d.ProjectNamespace+"#"+d.ProjectName)
			_, _, err := d.CommandRunner.Run(d.ProjectPath, step.Shell, commands)
			if err != nil {
				return false, fmt.Errorf("error running command: %v", err)
			}
		}
	}
	return true, nil
}

func cleanupTerraformOutput(nonEmptyOutput bool, planError error, stdout string, stderr string, regexStr *string) string {
	var errorStr, start string

	// removes output of terraform -version command that terraform-exec executes on every run
	i := strings.Index(stdout, "Initializing the backend...")
	if i != -1 {
		stdout = stdout[i:]
	}
	endPos := len(stdout)

	if planError != nil {
		if stderr != "" {
			errorStr = stderr
		} else if stdout != "" {
			errorStr = stdout
		}
		return errorStr
	} else if nonEmptyOutput {
		start = "Terraform will perform the following actions:"
	} else {
		start = "No changes. Your infrastructure matches the configuration."
	}

	startPos := strings.Index(stdout, start)
	if startPos == -1 {
		startPos = 0
	}

	if regexStr != nil {
		regex := regexp.MustCompile(*regexStr)
		matches := regex.FindStringSubmatch(stdout)
		if len(matches) > 0 {
			firstMatch := matches[0]
			endPos = strings.LastIndex(stdout, firstMatch) + len(firstMatch)
		}
	}

	// This should not happen but in case we get here we avoid slice bounds out of range exception by resetting endPos
	if endPos <= startPos {
		endPos = len(stdout)
	}
	return stdout[startPos:endPos]
}

func cleanupTerraformApply(nonEmptyPlan bool, planError error, stdout string, stderr string) string {
	return cleanupTerraformOutput(nonEmptyPlan, planError, stdout, stderr, nil)
}

func cleanupTerraformPlan(nonEmptyPlan bool, planError error, stdout string, stderr string) string {
	regex := `───────────.+`
	return cleanupTerraformOutput(nonEmptyPlan, planError, stdout, stderr, &regex)
}
