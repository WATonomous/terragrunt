package scaffold

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-github/v35/github"
	"golang.org/x/oauth2"

	"github.com/gruntwork-io/terragrunt/terraform"

	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/gruntwork-io/terragrunt/cli/commands/hclfmt"
	"github.com/gruntwork-io/terragrunt/util"

	"github.com/gruntwork-io/go-commons/errors"
	"github.com/gruntwork-io/terratest/modules/files"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	boilerplate_options "github.com/gruntwork-io/boilerplate/options"
	"github.com/gruntwork-io/boilerplate/templates"
	"github.com/gruntwork-io/boilerplate/variables"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/hashicorp/go-getter"
)

const (
	SourceUrlTypeHttps = "git-https"
	SourceUrlTypeGit   = "git-ssh"
	SourceGitSshUser   = "git"

	defaultBoilerplateConfig = `
variables:
  - name: SourceUrlType
    description: Source URL type, https, git
    default: https
  - name: SourceGitSshUser
    description: Default git SSH user
    default: git
`
	defaultTerragruntTemplate = `
# This is a Terragrunt module generated by boilerplate.
terraform {
  source = "{{ .sourceUrl }}"
}

inputs = {
  # --------------------------------------------------------------------------------------------------------------------
  # Required input variables
  # --------------------------------------------------------------------------------------------------------------------
  {{range .parsedRequiredInputs}}
  # Description: {{ .Description }}
  # Type: {{ .Type }}
  {{.Name}} = null  # TODO: fill in value
  {{end}}

  # --------------------------------------------------------------------------------------------------------------------
  # Optional input variables
  # Uncomment the ones you wish to set
  # --------------------------------------------------------------------------------------------------------------------
  {{range .parsedOptionalInputs}}
  # Description: {{ .Description }}
  # Type: {{ .Type }}
  # {{.Name}} = {{.DefaultValue}}
  {{end}}
}
`
)

func Run(opts *options.TerragruntOptions) error {
	// download remote repo to local
	moduleUrl := ""
	templateUrl := ""
	if len(opts.TerraformCliArgs) >= 2 {
		moduleUrl = opts.TerraformCliArgs[1]
	}

	if len(opts.TerraformCliArgs) >= 3 {
		templateUrl = opts.TerraformCliArgs[2]
	}

	tempDir, err := os.MkdirTemp("", "scaffold")
	if err != nil {
		return errors.WithStackTrace(err)
	}

	parsedModuleUrl, err := terraform.ToSourceUrl(moduleUrl, tempDir)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	params := parsedModuleUrl.Query()
	ref := params.Get("ref")
	if ref == "" {
		rootSourceUrl, _, err := terraform.SplitSourceUrl(parsedModuleUrl, opts.Logger)
		if err != nil {
			return errors.WithStackTrace(err)
		}

		tag, err := getLatestReleaseTag(rootSourceUrl)
		if err == nil {
			params.Add("ref", tag)
			parsedModuleUrl.RawQuery = params.Encode()
		}
	}

	moduleUrl = parsedModuleUrl.String()
	opts.Logger.Infof("Scaffolding a new Terragrunt module %s %s to %s", parsedModuleUrl.String(), templateUrl, opts.WorkingDir)

	if err := getter.GetAny(tempDir, parsedModuleUrl.String()); err != nil {
		return errors.WithStackTrace(err)
	}
	if err != nil {
		return errors.WithStackTrace(err)
	}

	templateDir := ""
	if templateUrl != "" {
		parsedTemplateUrl, err := terraform.ToSourceUrl(templateUrl, tempDir)

		templateDir, err = os.MkdirTemp("", "templateDir")
		if err != nil {
			return errors.WithStackTrace(err)
		}
		err = getter.GetAny(templateDir, parsedTemplateUrl.String())
		if err != nil {
			return errors.WithStackTrace(err)
		}
	}

	inputs, err := listInputs(opts, tempDir)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	// run boilerplate

	// prepare boilerplate dir
	boilerplateDir := util.JoinPath(tempDir, util.DefaultBoilerplateDir)

	// use template dir as boilerplate dir
	if templateDir != "" {
		boilerplateDir = templateDir
	}

	if !files.IsExistingDir(boilerplateDir) {
		// no default boilerplate dir, create one
		boilerplateDir, err = os.MkdirTemp("", "scaffold")
		if err != nil {
			return errors.WithStackTrace(err)
		}
		err = os.WriteFile(util.JoinPath(boilerplateDir, "terragrunt.hcl"), []byte(defaultTerragruntTemplate), 0644)
		if err != nil {
			return errors.WithStackTrace(err)
		}
		err = os.WriteFile(util.JoinPath(boilerplateDir, "boilerplate.yml"), []byte(defaultBoilerplateConfig), 0644)
		if err != nil {
			return errors.WithStackTrace(err)
		}
	}

	// prepare inputs
	vars, err := variables.ParseVars(opts.ScaffoldVars, opts.ScaffoldVarFiles)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	// separate inputs that require value and with default value
	var parsedRequiredInputs []*ParsedInput
	var parsedOptionalInputs []*ParsedInput

	for _, value := range inputs {
		if value.DefaultValue == "" {
			parsedRequiredInputs = append(parsedRequiredInputs, value)
		} else {
			parsedOptionalInputs = append(parsedOptionalInputs, value)
		}
	}

	vars["parsedRequiredInputs"] = parsedRequiredInputs
	vars["parsedOptionalInputs"] = parsedOptionalInputs

	// prepare source url

	sourceUrlType := SourceUrlTypeHttps
	if value, found := vars["SourceUrlType"]; found {
		sourceUrlType = fmt.Sprintf("%s", value)
	}

	scheme, host, path := parseUrl(opts, moduleUrl)
	// try to rewrite module url if is https and is requested to be git
	if scheme != "" {
		if scheme == SourceUrlTypeHttps && sourceUrlType == SourceUrlTypeGit {
			// TODO: handle git -> https
			gitUser := SourceGitSshUser
			if value, found := vars["SourceGitSshUser"]; found {
				gitUser = fmt.Sprintf("%s", value)
			}
			if strings.HasPrefix(path, "/") {
				path = path[1:]
			}

			moduleUrl = fmt.Sprintf("%s@%s:%s", gitUser, host, path)
		}
	}

	data, err := url.Parse(moduleUrl)
	fmt.Printf("data: %v\n", data)

	vars["sourceUrl"] = moduleUrl

	opts.Logger.Infof("Running boilerplate in %s", opts.WorkingDir)
	boilerplateOpts := &boilerplate_options.BoilerplateOptions{
		TemplateFolder:  boilerplateDir,
		OutputFolder:    opts.WorkingDir,
		OnMissingKey:    boilerplate_options.DefaultMissingKeyAction,
		OnMissingConfig: boilerplate_options.DefaultMissingConfigAction,
		Vars:            vars,

		NonInteractive: true,
	}
	emptyDep := variables.Dependency{}
	err = templates.ProcessTemplate(boilerplateOpts, boilerplateOpts, emptyDep)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	// running fmt
	err = hclfmt.Run(opts)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	return nil
}

// token := os.Getenv("GITHUB_OAUTH_TOKEN")

func getLatestReleaseTag(parsedURL *url.URL) (string, error) {
	pathParts := strings.Split(parsedURL.Path, "/")
	if len(pathParts) < 2 {
		return "", fmt.Errorf("invalid repository URL")
	}
	owner := pathParts[1]
	repo := pathParts[2]

	repo = strings.TrimSuffix(repo, ".git")

	token := os.Getenv("GITHUB_OAUTH_TOKEN")

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)

	release, _, err := client.Repositories.GetLatestRelease(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	tagName := release.GetTagName()
	return tagName, nil
}

func parseUrl(opts *options.TerragruntOptions, moduleUrl string) (string, string, string) {
	pattern := `git::([^:]+)://([^/]+)(/.*)`

	re := regexp.MustCompile(pattern)

	matches := re.FindStringSubmatch(moduleUrl)
	if len(matches) != 4 {
		opts.Logger.Warnf("Failed to parse module url %s", moduleUrl)
		return "", "", ""
	}

	scheme := matches[1]
	host := matches[2]
	path := matches[3]

	return scheme, host, path
}

// ParsedInput structure with input name, default value and description.
type ParsedInput struct {
	Name         string
	Description  string
	Type         string
	DefaultValue string
}

func listInputs(opts *options.TerragruntOptions, directoryPath string) ([]*ParsedInput, error) {
	tfFiles, err := listTerraformFiles(directoryPath)
	if err != nil {
		return nil, errors.WithStackTrace(err)
	}
	parser := hclparse.NewParser()

	// Extract variables from all TF files
	var parsedInputs []*ParsedInput
	for _, tfFile := range tfFiles {
		content, err := os.ReadFile(tfFile)
		if err != nil {
			opts.Logger.Errorf("Error reading file %s: %v", tfFile, err)
			continue
		}
		file, diags := parser.ParseHCL(content, tfFile)
		if diags.HasErrors() {
			opts.Logger.Warnf("Failed to parse HCL in file %s: %v", tfFile, diags)
			continue
		}

		ctx := &hcl.EvalContext{}

		if body, ok := file.Body.(*hclsyntax.Body); ok {
			for _, block := range body.Blocks {
				if block.Type == "variable" {
					if len(block.Labels[0]) > 0 {

						name := block.Labels[0]
						descriptionAttr, err := readBlockAttribute(ctx, block, "description")
						descriptionAttrText := ""
						if err != nil {
							opts.Logger.Warnf("Failed to read descriptionAttr for %s %v", name, err)
							descriptionAttr = nil
						}
						if descriptionAttr != nil {
							descriptionAttrText = descriptionAttr.AsString()
						} else {
							descriptionAttrText = fmt.Sprintf("No description for %s", name)
						}

						typeAttr, err := readBlockAttribute(ctx, block, "type")
						typeAttrText := ""
						if err != nil {
							opts.Logger.Warnf("Failed to read type attribute for %s %v", name, err)
							descriptionAttr = nil
						}
						if typeAttr != nil {
							typeAttrText = typeAttr.AsString()
						} else {
							typeAttrText = fmt.Sprintf("No type for %s", name)
						}

						defaultValue, err := readBlockAttribute(ctx, block, "default")
						if err != nil {
							opts.Logger.Warnf("Failed to read default value for %s %v", name, err)
							defaultValue = nil
						}

						defaultValueText := ""
						if defaultValue != nil {
							jsonBytes, err := ctyjson.Marshal(*defaultValue, cty.DynamicPseudoType)
							if err != nil {
								return nil, errors.WithStackTrace(err)
							}

							var ctyJsonOutput CtyJsonValue
							if err := json.Unmarshal(jsonBytes, &ctyJsonOutput); err != nil {
								return nil, errors.WithStackTrace(err)
							}

							jsonBytes, err = json.Marshal(ctyJsonOutput.Value)
							if err != nil {
								return nil, errors.WithStackTrace(err)
							}
							defaultValueText = string(jsonBytes)
						}

						input := &ParsedInput{
							Name:         name,
							Type:         typeAttrText,
							Description:  descriptionAttrText,
							DefaultValue: defaultValueText,
						}

						parsedInputs = append(parsedInputs, input)
					}
				}
			}
		}
	}
	return parsedInputs, nil
}

type CtyJsonValue struct {
	Value interface{}
	Type  interface{}
}

func readBlockAttribute(ctx *hcl.EvalContext, block *hclsyntax.Block, name string) (*cty.Value, error) {
	if attr, ok := block.Body.Attributes[name]; ok {
		if attr.Expr != nil {
			// check if first var is traversal
			if len(attr.Expr.Variables()) > 0 {
				v := attr.Expr.Variables()[0]
				// check if variable is traversal
				if varTr, ok := v[0].(hcl.TraverseRoot); ok {
					result := cty.StringVal(varTr.Name)
					return &result, nil
				}
			}

			value, err := attr.Expr.Value(ctx)
			if err != nil {
				return nil, err
			}
			return &value, nil
		}
	}
	return nil, nil
}

// listTerraformFiles returns a list of all TF files in the specified directory.
func listTerraformFiles(directoryPath string) ([]string, error) {
	var tfFiles []string

	err := filepath.Walk(directoryPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".tf" {
			tfFiles = append(tfFiles, path)
		}
		return nil
	})

	return tfFiles, err
}
