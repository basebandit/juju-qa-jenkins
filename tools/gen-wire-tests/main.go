package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/mvdan/sh/syntax"
	"gopkg.in/yaml.v2"
)

// Config represents the different ways to config the linter
type Config struct {
	Folders struct {
		Skip         []string                       `yaml:"skip-all"`
		SkipLXD      []string                       `yaml:"skip-lxd"`
		SkipAWS      []string                       `yaml:"skip-aws"`
		SkipGoogle   []string                       `yaml:"skip-google"`
		SkipAzure    []string                       `yaml:"skip-azure"`
		SkipMicrok8s []string                       `yaml:"skip-microk8s"`
		SkipSubTasks []string                       `yaml:"skip-subtasks"`
		PreventSplit []string                       `yaml:"prevent-split"`
		Ephemeral    []string                       `yaml:"ephemeral"`
		Unstable     map[string]map[string][]string `yaml:"unstable"`
		Timeout      map[string]map[string]int      `yaml:"timeout"`
	}
}

type Task struct {
	Clouds                   []Cloud
	SubTasks                 []string
	UnstableProviderSubTasks map[string][]string
	ExcludedTasks            []string
	Timeout                  map[string]int
}

type Cloud struct {
	CloudName    string
	ProviderName string
	Region       string
}

var (
	lxd      = Cloud{CloudName: "lxd", ProviderName: "lxd"}
	aws      = Cloud{CloudName: "aws", ProviderName: "aws", Region: "us-east-1"}
	google   = Cloud{CloudName: "google", ProviderName: "google", Region: "us-east1"}
	azure    = Cloud{CloudName: "azure", ProviderName: "azure", Region: "centralus"}
	microk8s = Cloud{CloudName: "microk8s", ProviderName: "k8s"}
)

// Gen-wire-tests will generate the integration test files for the juju
// integration tests. This will help prevent wire up mistakes or any missing
// test suite tests.
//
// It expects two arguments to be passed in:
// - inputDir: the juju test suite location
// - outputDir: the location of the new jenkins config files
//
// Additionally it expects a config file passed in via stdin, this allows the
// configuration of the gen-wire-tests. In reality it allows the skipping of
// folders that are custom and don't follow the generic setup.
func main() {
	if len(os.Args) < 3 {
		log.Fatal("expected directory argument only.")
	}
	inputDir := os.Args[1]
	outputDir := os.Args[2]

	if outDir, err := os.Open(outputDir); os.IsNotExist(err) {
		if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
			log.Fatal("unable to create output dir", outputDir)
		}
	} else {
		log.Println("Warning: Output Directory already exists. It may overwrite files!")
		// Remove all yaml files so that git can track deleted files as well as new ones.
		outFiles, err := outDir.Readdirnames(0)
		if err != nil {
			log.Fatalf("unable to read output dir files: %v", err)
		}
		for _, f := range outFiles {
			if !strings.HasSuffix(f, ".yml") {
				continue
			}
			if err := os.Remove(filepath.Join(outputDir, f)); err != nil {
				log.Fatalf("unable to remove existing file %q: %v", f, err)
			}
		}
	}

	reader := bufio.NewReader(os.Stdin)
	bytes, err := ioutil.ReadAll(reader)
	if err != nil {
		log.Fatal("unexpected config", err)
	}
	var config Config
	if err := yaml.Unmarshal(bytes, &config); err != nil {
		log.Fatal("config parse error", err)
	}

	dirs, err := ioutil.ReadDir(inputDir)
	if err != nil {
		log.Fatalf("unable to get listing dir %q with error %v", inputDir, err)
	}

	var suiteNames []string
	testSuites := make(map[string]Task)
	for _, dir := range dirs {
		suiteName := dir.Name()
		if contains(config.Folders.Skip, suiteName) {
			continue
		}
		// Expose all non skipped sub-tasks!
		taskNames := []string{suiteName}
		excluded := []string{}
		if !contains(config.Folders.PreventSplit, suiteName) {
			taskNames = []string{}
			subTaskNames := parseTaskNames(inputDir, dir)
			for _, subTask := range subTaskNames {
				if !contains(config.Folders.SkipSubTasks, subTask) {
					taskNames = append(taskNames, subTask)
				} else {
					excluded = append(excluded, subTask)
				}
			}
		}

		suiteNames = append(suiteNames, suiteName)

		clouds := make([]Cloud, 0)
		if !contains(config.Folders.SkipLXD, suiteName) {
			clouds = append(clouds, lxd)
		}
		if !contains(config.Folders.SkipAWS, suiteName) {
			clouds = append(clouds, aws)
		}
		if !contains(config.Folders.SkipGoogle, suiteName) {
			clouds = append(clouds, google)
		}
		if !contains(config.Folders.SkipAzure, suiteName) {
			clouds = append(clouds, azure)
		}
		if !contains(config.Folders.SkipMicrok8s, suiteName) {
			clouds = append(clouds, microk8s)
		}

		testSuites[suiteName] = Task{
			Clouds:                   clouds,
			SubTasks:                 taskNames,
			UnstableProviderSubTasks: config.Folders.Unstable[suiteName],
			ExcludedTasks:            excluded,
			Timeout:                  config.Folders.Timeout[suiteName],
		}
	}

	funcMap := map[string]interface{}{
		"ensureHyphen": func(s string) string {
			return strings.ReplaceAll(s, "_", "-")
		},
		"contains": func(arr []string, s string) bool {
			for _, v := range arr {
				if s == v {
					return true
				}
			}
			return false
		},
	}
	t := template.Must(template.New("integration").Funcs(funcMap).Parse(Template))

	for _, name := range suiteNames {
		task := testSuites[name]
		writeJobDefinitions(t, config, outputDir, task, name, false)
		if len(task.UnstableProviderSubTasks) > 0 {
			writeJobDefinitions(t, config, outputDir, task, name, true)
		}
	}
}

func writeJobDefinitions(
	t *template.Template,
	config Config,
	outputDir string,
	task Task,
	suiteName string,
	unstable bool,
) {
	unstableLabel := ""
	if unstable {
		unstableLabel = "-unstable"
	}
	outputPath := filepath.Join(outputDir, fmt.Sprintf("test-%s%s.yml", suiteName, unstableLabel))
	f, err := os.Create(outputPath)
	if err != nil {
		log.Fatal("unable to create output file", outputPath)
	}
	defer f.Close()

	skipTasks := make([][]string, len(task.SubTasks))
	for k := range task.SubTasks {
		for x, y := range task.SubTasks {
			if k == x {
				continue
			}
			skipTasks[k] = append(skipTasks[k], y)
		}
		sort.Slice(skipTasks[k], func(i, j int) bool {
			return skipTasks[k][i] < skipTasks[k][j]
		})
	}
	joined := make([]string, len(skipTasks))
	for k, v := range skipTasks {
		v = append(v, task.ExcludedTasks...)
		joined[k] = strings.Join(v, ",")
	}

	ephemeral := make(map[string]bool)
	for _, test := range config.Folders.Ephemeral {
		ephemeral[test] = true
	}

	if err := t.Execute(f, struct {
		SuiteName     string
		Clouds        []Cloud
		TaskNames     []string
		SkipTasks     []string
		ExcludedTasks string
		UnstableTasks map[string][]string
		Unstable      bool
		Ephemeral     map[string]bool
		Timeout       map[string]int
	}{
		SuiteName:     suiteName,
		Clouds:        task.Clouds,
		TaskNames:     task.SubTasks,
		SkipTasks:     joined,
		ExcludedTasks: strings.Join(task.ExcludedTasks, ","),
		UnstableTasks: task.UnstableProviderSubTasks,
		Unstable:      unstable,
		Ephemeral:     ephemeral,
		Timeout:       task.Timeout,
	}); err != nil {
		log.Fatalf("unable to execute template %q with error %v", suiteName, err)
	}
	f.Sync()
}

func parseTaskNames(rootDir string, dir os.FileInfo) []string {
	tasks := make(map[string]int)

	leaf := filepath.Join(rootDir, dir.Name())
	filepath.Walk(leaf, func(s string, d os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		matched, err := regexp.Match("^"+leaf+"/\\w+.sh$", []byte(s))
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}

		// Now we've got a match, read the file.
		file, err := os.Open(filepath.Join(leaf, d.Name()))
		if err != nil {
			return err
		}

		parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
		prog, err := parser.Parse(file, d.Name())
		if err != nil {
			return err
		}
		syntax.Walk(prog, func(node syntax.Node) bool {
			switch t := node.(type) {
			case *syntax.FuncDecl:
				// Capture the name of the function.
				if !strings.HasPrefix(t.Name.Value, "test_") {
					return true
				}

				// Traverse the function body to ensure that anything with
				// test is called.
				if _, ok := tasks[t.Name.Value]; !ok {
					tasks[t.Name.Value] = 1
				} else {
					tasks[t.Name.Value]++
				}

				syntax.Walk(t.Body, func(node syntax.Node) bool {
					switch t := node.(type) {
					case *syntax.CallExpr:
						// We're not interested in items called outside of our
						// function case.
						if len(t.Args) == 0 {
							return true
						}
						for _, arg := range t.Args {
							lit, ok := arg.Parts[0].(*syntax.Lit)
							if !ok || !strings.HasPrefix(lit.Value, "test_") {
								return true
							}
							if _, ok := tasks[lit.Value]; !ok {
								tasks[lit.Value] = 1
							} else {
								tasks[lit.Value]++
							}
						}

						return true
					}
					return true
				})

			}
			return true
		})
		if err != nil {
			return err
		}

		return nil
	})

	subtasks := make([]string, 0, len(tasks))
	for name, count := range tasks {
		if count < 2 {
			continue
		}
		subtasks = append(subtasks, name)
	}
	sort.Strings(subtasks)
	return subtasks
}

func contains(haystack []string, needle string) bool {
	for _, skip := range haystack {
		if needle == skip {
			return true
		}
	}
	return false
}

// Template represents the integration test configuration for jenkins job
// builder to run.
const Template = `
{{$node := .}}
# Code generated by gen-wire-tests. DO NOT EDIT.
- job:
    {{- if eq $node.Unstable true}}
    name: 'test-{{$.SuiteName}}-unstable-multijob'
    {{- else}}
    name: 'test-{{$.SuiteName}}-multijob'
    {{- end}}
    project-type: 'multijob'
    description: |-
        Test {{.SuiteName}} Suite
    condition: SUCCESSFUL
    node: noop-parent-jobs
    concurrent: true
    wrappers:
    - ansicolor
    - workspace-cleanup
    - timestamps
    parameters:
    - string:
        default: ''
        description: 'Enable sub job to be run individually.'
        name: SHORT_GIT_COMMIT
    - string:
        default: ''
        description: 'Build arch used to download the build tar.gz.'
        name: BUILD_ARCH
    - string:
        default: ''
        description: 'Ubuntu series to use when bootstrapping Juju'
        name: BOOTSTRAP_SERIES
    builders:
    - get-build-details
    - set-test-description
    - multijob:
        name: 'IntegrationTests-{{.SuiteName}}'
        projects:
{{- range $k, $skip_tasks := $node.SkipTasks}}
{{- range $cloud := $node.Clouds}}
    {{- $unstableTasks := index $node.UnstableTasks $cloud.ProviderName -}}
    {{- $task_name := index $node.TaskNames $k -}}
    {{- if eq (len $node.SkipTasks) 1}}
        - name: 'test-{{$.SuiteName}}-{{$cloud.CloudName}}'
          current-parameters: true
    {{- else}}
      {{- if eq (contains $unstableTasks $task_name) $node.Unstable}}
        - name: 'test-{{$.SuiteName}}-{{ensureHyphen $task_name}}-{{$cloud.CloudName}}'
          current-parameters: true
      {{- end -}}
    {{- end -}}
{{- end}}
{{- end}}

{{- range $k, $skip_tasks := $node.SkipTasks -}}
{{- range $cloud := $node.Clouds -}}
    {{- $task_name := "" -}}
    {{- $test_name := (printf "%s-%s" $.SuiteName $cloud.CloudName) -}}
    {{- $full_task_name := (printf "test-%s" $test_name) -}}
    {{- if gt (len $node.SkipTasks) 1 }}
        {{- $task_name = index $node.TaskNames $k -}}
        {{- $full_task_name = (printf "test-%s-%s-%s" $.SuiteName (ensureHyphen $task_name) $cloud.CloudName) -}}
    {{- end }}

    {{- $builder := "run-integration-test" -}}
    {{- $run_on := "ephemeral-focal-small-amd64" -}}
    {{- if or (eq (index $node.Ephemeral $test_name) true) (eq $cloud.ProviderName "lxd") }}
      {{- $builder = "run-integration-test" -}}
      {{- $run_on = "ephemeral-focal-8c-32g-amd64" -}}
    {{- end }}
    {{- if eq $cloud.CloudName "microk8s" }}
      {{- $builder = "run-integration-test-microk8s" -}}
      {{- $run_on = "ephemeral-focal-8c-32g-amd64" -}}
    {{- end }}

{{- if eq $node.Unstable false }}
{{$timeout := (index $node.Timeout $task_name)}}
- job:
    name: {{$full_task_name}}
    node: {{$run_on}}
    description: |-
    {{- if eq (len $node.SkipTasks) 1 }}
      Test {{$.SuiteName}} suite on {{$cloud.CloudName}}
    {{ else }}
      Test {{$task_name}} in {{$.SuiteName}} suite on {{$cloud.CloudName}}
    {{ end -}}
    parameters:
    - validating-string:
        name: SHORT_GIT_COMMIT
        description: 'Enable sub job to be run individually.'
        regex: ^\S{7}$
        msg: Enter a valid 7 char git sha
    - choice:
        default: 'amd64'
        description: 'Build arch used to download the build tar.gz.'
        name: BUILD_ARCH
        choices:
        - amd64
        - arm64
        - s390x
        - ppc64el
    - string:
        default: '{{$cloud.CloudName}}'
        description: 'Cloud to use when bootstrapping Juju'
        name: BOOTSTRAP_CLOUD
    - string:
        default: '{{$cloud.ProviderName}}'
        description: 'Provider to use when bootstrapping Juju'
        name: BOOTSTRAP_PROVIDER
{{- if $cloud.Region }}
    - string:
        default: '{{$cloud.Region}}'
        description: 'Cloud Region to use when bootstrapping Juju'
        name: BOOTSTRAP_REGION
{{- end }}
    - string:
        default: ''
        description: 'Ubuntu series to use when bootstrapping Juju'
        name: BOOTSTRAP_SERIES
    - string:
        default: jujuqabot
        description: "Operator docker image account name."
        name: OPERATOR_IMAGE_ACCOUNT
    wrappers:
      - default-integration-test-wrapper
      - timeout:
          timeout: {{- if gt $timeout 0 }} {{$timeout}}{{ else }} 50{{- end}}
          fail: true
          type: absolute
    builders:
      - {{$builder}}:
            test_name: '{{$.SuiteName}}'
            setup_steps: ''
{{- if gt (len $node.SkipTasks) 1 }}
            task_name: '{{$task_name}}'
            skip_tasks: '{{$skip_tasks}}'
{{ else }}
            task_name: ''
            skip_tasks: '{{$node.ExcludedTasks}}'
{{- end}}
    publishers:
      - integration-artifacts
{{- end -}}
{{- end }}
{{- end }}
`
