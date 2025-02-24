package state

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/roboll/helmfile/pkg/environment"
	"github.com/roboll/helmfile/pkg/event"
	"github.com/roboll/helmfile/pkg/helmexec"
	"github.com/roboll/helmfile/pkg/remote"
	"github.com/roboll/helmfile/pkg/tmpl"

	"github.com/tatsushid/go-prettytable"
	"github.com/variantdev/vals"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

// HelmState structure for the helmfile
type HelmState struct {
	basePath string
	FilePath string

	// DefaultValues is the default values to be overrode by environment values and command-line overrides
	DefaultValues []interface{} `yaml:"values,omitempty"`

	Environments map[string]EnvironmentSpec `yaml:"environments,omitempty"`

	Bases              []string          `yaml:"bases,omitempty"`
	HelmDefaults       HelmSpec          `yaml:"helmDefaults,omitempty"`
	Helmfiles          []SubHelmfileSpec `yaml:"helmfiles,omitempty"`
	DeprecatedContext  string            `yaml:"context,omitempty"`
	DeprecatedReleases []ReleaseSpec     `yaml:"charts,omitempty"`
	OverrideNamespace  string            `yaml:"namespace,omitempty"`
	Repositories       []RepositorySpec  `yaml:"repositories,omitempty"`
	Releases           []ReleaseSpec     `yaml:"releases,omitempty"`
	Selectors          []string          `yaml:"-"`

	Templates map[string]TemplateSpec `yaml:"templates"`

	Env environment.Environment `yaml:"-"`

	logger *zap.SugaredLogger

	readFile func(string) ([]byte, error)

	removeFile func(string) error
	fileExists func(string) (bool, error)
	glob       func(string) ([]string, error)
	tempDir    func(string, string) (string, error)

	runner      helmexec.Runner
	helm        helmexec.Interface
	valsRuntime vals.Evaluator
}

// SubHelmfileSpec defines the subhelmfile path and options
type SubHelmfileSpec struct {
	//path or glob pattern for the sub helmfiles
	Path string `yaml:"path,omitempty"`
	//chosen selectors for the sub helmfiles
	Selectors []string `yaml:"selectors,omitempty"`
	//do the sub helmfiles inherits from parent selectors
	SelectorsInherited bool `yaml:"selectorsInherited,omitempty"`

	Environment SubhelmfileEnvironmentSpec
}

type SubhelmfileEnvironmentSpec struct {
	OverrideValues []interface{} `yaml:"values,omitempty"`
}

// HelmSpec to defines helmDefault values
type HelmSpec struct {
	KubeContext     string   `yaml:"kubeContext,omitempty"`
	TillerNamespace string   `yaml:"tillerNamespace,omitempty"`
	Tillerless      bool     `yaml:"tillerless"`
	Args            []string `yaml:"args,omitempty"`
	Verify          bool     `yaml:"verify"`
	// Devel, when set to true, use development versions, too. Equivalent to version '>0.0.0-0'
	Devel bool `yaml:"devel"`
	// Wait, if set to true, will wait until all Pods, PVCs, Services, and minimum number of Pods of a Deployment are in a ready state before marking the release as successful
	Wait bool `yaml:"wait"`
	// Timeout is the time in seconds to wait for any individual Kubernetes operation (like Jobs for hooks, and waits on pod/pvc/svc/deployment readiness) (default 300)
	Timeout int `yaml:"timeout"`
	// RecreatePods, when set to true, instruct helmfile to perform pods restart for the resource if applicable
	RecreatePods bool `yaml:"recreatePods"`
	// Force, when set to true, forces resource update through delete/recreate if needed
	Force bool `yaml:"force"`
	// Atomic, when set to true, restore previous state in case of a failed install/upgrade attempt
	Atomic bool `yaml:"atomic"`

	TLS       bool   `yaml:"tls"`
	TLSCACert string `yaml:"tlsCACert,omitempty"`
	TLSKey    string `yaml:"tlsKey,omitempty"`
	TLSCert   string `yaml:"tlsCert,omitempty"`
}

// RepositorySpec that defines values for a helm repo
type RepositorySpec struct {
	Name     string `yaml:"name,omitempty"`
	URL      string `yaml:"url,omitempty"`
	CaFile   string `yaml:"caFile,omitempty"`
	CertFile string `yaml:"certFile,omitempty"`
	KeyFile  string `yaml:"keyFile,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// ReleaseSpec defines the structure of a helm release
type ReleaseSpec struct {
	// Chart is the name of the chart being installed to create this release
	Chart   string `yaml:"chart,omitempty"`
	Version string `yaml:"version,omitempty"`
	Verify  *bool  `yaml:"verify,omitempty"`
	// Devel, when set to true, use development versions, too. Equivalent to version '>0.0.0-0'
	Devel *bool `yaml:"devel,omitempty"`
	// Wait, if set to true, will wait until all Pods, PVCs, Services, and minimum number of Pods of a Deployment are in a ready state before marking the release as successful
	Wait *bool `yaml:"wait,omitempty"`
	// Timeout is the time in seconds to wait for any individual Kubernetes operation (like Jobs for hooks, and waits on pod/pvc/svc/deployment readiness) (default 300)
	Timeout *int `yaml:"timeout,omitempty"`
	// RecreatePods, when set to true, instruct helmfile to perform pods restart for the resource if applicable
	RecreatePods *bool `yaml:"recreatePods,omitempty"`
	// Force, when set to true, forces resource update through delete/recreate if needed
	Force *bool `yaml:"force,omitempty"`
	// Installed, when set to true, `delete --purge` the release
	Installed *bool `yaml:"installed,omitempty"`
	// Atomic, when set to true, restore previous state in case of a failed install/upgrade attempt
	Atomic *bool `yaml:"atomic,omitempty"`

	// MissingFileHandler is set to either "Error" or "Warn". "Error" instructs helmfile to fail when unable to find a values or secrets file. When "Warn", it prints the file and continues.
	// The default value for MissingFileHandler is "Error".
	MissingFileHandler *string `yaml:"missingFileHandler,omitempty"`
	// Needs is the [TILLER_NS/][NS/]NAME representations of releases that this release depends on.
	Needs []string `yaml:"needs,omitempty"`

	// Hooks is a list of extension points paired with operations, that are executed in specific points of the lifecycle of releases defined in helmfile
	Hooks []event.Hook `yaml:"hooks,omitempty"`

	// Name is the name of this release
	Name      string            `yaml:"name,omitempty"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
	Values    []interface{}     `yaml:"values,omitempty"`
	Secrets   []string          `yaml:"secrets,omitempty"`
	SetValues []SetValue        `yaml:"set,omitempty"`

	ValuesTemplate    []interface{} `yaml:"valuesTemplate,omitempty"`
	SetValuesTemplate []SetValue    `yaml:"setTemplate,omitempty"`

	// The 'env' section is not really necessary any longer, as 'set' would now provide the same functionality
	EnvValues []SetValue `yaml:"env,omitempty"`

	ValuesPathPrefix string `yaml:"valuesPathPrefix,omitempty"`

	TillerNamespace string `yaml:"tillerNamespace,omitempty"`
	Tillerless      *bool  `yaml:"tillerless,omitempty"`

	KubeContext string `yaml:"kubeContext,omitempty"`

	TLS       *bool  `yaml:"tls,omitempty"`
	TLSCACert string `yaml:"tlsCACert,omitempty"`
	TLSKey    string `yaml:"tlsKey,omitempty"`
	TLSCert   string `yaml:"tlsCert,omitempty"`

	// These values are used in templating
	TillerlessTemplate *string `yaml:"tillerlessTemplate,omitempty"`
	VerifyTemplate     *string `yaml:"verifyTemplate,omitempty"`
	WaitTemplate       *string `yaml:"waitTemplate,omitempty"`
	InstalledTemplate  *string `yaml:"installedTemplate,omitempty"`

	// These settings requires helm-x integration to work
	Dependencies          []Dependency  `yaml:"dependencies,omitempty"`
	JSONPatches           []interface{} `yaml:"jsonPatches,omitempty"`
	StrategicMergePatches []interface{} `yaml:"strategicMergePatches,omitempty"`
	Adopt                 []string      `yaml:"adopt,omitempty"`

	// generatedValues are values that need cleaned up on exit
	generatedValues []string
	//version of the chart that has really been installed cause desired version may be fuzzy (~2.0.0)
	installedVersion string
}

type Release struct {
	ReleaseSpec

	Filtered bool
}

// SetValue are the key values to set on a helm release
type SetValue struct {
	Name   string   `yaml:"name,omitempty"`
	Value  string   `yaml:"value,omitempty"`
	File   string   `yaml:"file,omitempty"`
	Values []string `yaml:"values,omitempty"`
}

// AffectedReleases hold the list of released that where updated, deleted, or in error
type AffectedReleases struct {
	Upgraded []*ReleaseSpec
	Deleted  []*ReleaseSpec
	Failed   []*ReleaseSpec
}

const DefaultEnv = "default"

const MissingFileHandlerError = "Error"
const MissingFileHandlerInfo = "Info"
const MissingFileHandlerWarn = "Warn"
const MissingFileHandlerDebug = "Debug"

func (st *HelmState) ApplyOverrides(spec *ReleaseSpec) {
	if st.OverrideNamespace != "" {
		spec.Namespace = st.OverrideNamespace

		for i := 0; i < len(spec.Needs); i++ {
			n := spec.Needs[i]
			if len(strings.Split(n, "/")) == 1 {
				spec.Needs[i] = st.OverrideNamespace + "/" + n
			}
		}
	}
}

type RepoUpdater interface {
	AddRepo(name, repository, cafile, certfile, keyfile, username, password string) error
	UpdateRepo() error
}

// SyncRepos will update the given helm releases
func (st *HelmState) SyncRepos(helm RepoUpdater) []error {
	errs := []error{}

	for _, repo := range st.Repositories {
		if err := helm.AddRepo(repo.Name, repo.URL, repo.CaFile, repo.CertFile, repo.KeyFile, repo.Username, repo.Password); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) != 0 {
		return errs
	}

	if err := helm.UpdateRepo(); err != nil {
		return []error{err}
	}
	return nil
}

type syncResult struct {
	errors []*ReleaseError
}

type syncPrepareResult struct {
	release *ReleaseSpec
	flags   []string
	errors  []*ReleaseError
}

// SyncReleases wrapper for executing helm upgrade on the releases
func (st *HelmState) prepareSyncReleases(helm helmexec.Interface, additionalValues []string, concurrency int, opt ...SyncOpt) ([]syncPrepareResult, []error) {
	opts := &SyncOpts{}
	for _, o := range opt {
		o.Apply(opts)
	}

	releases := []*ReleaseSpec{}
	for i, _ := range st.Releases {
		releases = append(releases, &st.Releases[i])
	}

	numReleases := len(releases)
	jobs := make(chan *ReleaseSpec, numReleases)
	results := make(chan syncPrepareResult, numReleases)

	res := []syncPrepareResult{}
	errs := []error{}

	mut := sync.Mutex{}

	st.scatterGather(
		concurrency,
		numReleases,
		func() {
			for i := 0; i < numReleases; i++ {
				jobs <- releases[i]
			}
			close(jobs)
		},
		func(workerIndex int) {
			for release := range jobs {
				st.ApplyOverrides(release)

				// If `installed: false`, the only potential operation on this release would be uninstalling.
				// We skip generating values files in that case, because for an uninstall with `helm delete`, we don't need to those.
				// The values files are for `helm upgrade -f values.yaml` calls that happens when the release has `installed: true`.
				// This logic addresses:
				// - https://github.com/roboll/helmfile/issues/519
				// - https://github.com/roboll/helmfile/issues/616
				if !release.Desired() {
					results <- syncPrepareResult{release: release, flags: []string{}, errors: []*ReleaseError{}}
					continue
				}

				// TODO We need a long-term fix for this :)
				// See https://github.com/roboll/helmfile/issues/737
				mut.Lock()
				flags, flagsErr := st.flagsForUpgrade(helm, release, workerIndex)
				mut.Unlock()
				if flagsErr != nil {
					results <- syncPrepareResult{errors: []*ReleaseError{newReleaseError(release, flagsErr)}}
					continue
				}

				errs := []*ReleaseError{}
				for _, value := range additionalValues {
					valfile, err := filepath.Abs(value)
					if err != nil {
						errs = append(errs, newReleaseError(release, err))
					}

					ok, err := st.fileExists(valfile)
					if err != nil {
						errs = append(errs, newReleaseError(release, err))
					} else if !ok {
						errs = append(errs, newReleaseError(release, fmt.Errorf("file does not exist: %s", valfile)))
					}
					flags = append(flags, "--values", valfile)
				}

				if opts.Set != nil {
					for _, s := range opts.Set {
						flags = append(flags, "--set", s)
					}
				}

				if len(errs) > 0 {
					results <- syncPrepareResult{errors: errs}
					continue
				}

				results <- syncPrepareResult{release: release, flags: flags, errors: []*ReleaseError{}}
			}
		},
		func() {
			for i := 0; i < numReleases; {
				select {
				case r := <-results:
					for _, e := range r.errors {
						errs = append(errs, e)
					}
					res = append(res, r)
					i++
				}
			}
		},
	)

	return res, errs
}

func (st *HelmState) isReleaseInstalled(context helmexec.HelmContext, helm helmexec.Interface, release ReleaseSpec) (bool, error) {
	out, err := st.listReleases(context, helm, &release)
	if err != nil {
		return false, err
	} else if out != "" {
		return true, nil
	}
	return false, nil
}

func (st *HelmState) DetectReleasesToBeDeleted(helm helmexec.Interface, releases []ReleaseSpec) ([]ReleaseSpec, error) {
	detected := []ReleaseSpec{}
	for i := range releases {
		release := releases[i]

		if !release.Desired() {
			installed, err := st.isReleaseInstalled(st.createHelmContext(&release, 0), helm, release)
			if err != nil {
				return nil, err
			} else if installed {
				// Otherwise `release` messed up(https://github.com/roboll/helmfile/issues/554)
				r := release
				detected = append(detected, r)
			}
		}
	}
	return detected, nil
}

type SyncOpts struct {
	Set []string
}

type SyncOpt interface{ Apply(*SyncOpts) }

func (o *SyncOpts) Apply(opts *SyncOpts) {
	*opts = *o
}

func ReleaseToID(r *ReleaseSpec) string {
	var id string

	tns := r.TillerNamespace
	if tns != "" {
		id += tns + "/"
	}

	ns := r.Namespace
	if ns != "" {
		id += ns + "/"
	}

	id += r.Name

	return id
}

// DeleteReleasesForSync deletes releases that are marked for deletion
func (st *HelmState) DeleteReleasesForSync(affectedReleases *AffectedReleases, helm helmexec.Interface, workerLimit int) []error {
	errs := []error{}

	releases := st.Releases

	jobQueue := make(chan *ReleaseSpec, len(releases))
	results := make(chan syncResult, len(releases))
	if workerLimit == 0 {
		workerLimit = len(releases)
	}

	m := new(sync.Mutex)

	st.scatterGather(
		workerLimit,
		len(releases),
		func() {
			for i := 0; i < len(releases); i++ {
				jobQueue <- &releases[i]
			}
			close(jobQueue)
		},
		func(workerIndex int) {
			for release := range jobQueue {
				var relErr *ReleaseError
				context := st.createHelmContext(release, workerIndex)

				if _, err := st.triggerPresyncEvent(release, "sync"); err != nil {
					relErr = newReleaseError(release, err)
				} else {
					var args []string
					if isHelm3() {
						args = []string{}
					} else {
						args = []string{"--purge"}
					}
					deletionFlags := st.appendConnectionFlags(args, release)
					m.Lock()
					if err := helm.DeleteRelease(context, release.Name, deletionFlags...); err != nil {
						affectedReleases.Failed = append(affectedReleases.Failed, release)
						relErr = newReleaseError(release, err)
					} else {
						affectedReleases.Deleted = append(affectedReleases.Deleted, release)
					}
					m.Unlock()
				}

				if relErr == nil {
					results <- syncResult{}
				} else {
					results <- syncResult{errors: []*ReleaseError{relErr}}
				}

				if _, err := st.triggerPostsyncEvent(release, relErr, "sync"); err != nil {
					st.logger.Warnf("warn: %v\n", err)
				}

				if _, err := st.triggerCleanupEvent(release, "sync"); err != nil {
					st.logger.Warnf("warn: %v\n", err)
				}
			}
		},
		func() {
			for i := 0; i < len(releases); {
				select {
				case res := <-results:
					if len(res.errors) > 0 {
						for _, e := range res.errors {
							errs = append(errs, e)
						}
					}
				}
				i++
			}
		},
	)
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// SyncReleases wrapper for executing helm upgrade on the releases
func (st *HelmState) SyncReleases(affectedReleases *AffectedReleases, helm helmexec.Interface, additionalValues []string, workerLimit int, opt ...SyncOpt) []error {
	opts := &SyncOpts{}
	for _, o := range opt {
		o.Apply(opts)
	}

	preps, prepErrs := st.prepareSyncReleases(helm, additionalValues, workerLimit, opts)
	if len(prepErrs) > 0 {
		return prepErrs
	}

	errs := []error{}
	jobQueue := make(chan *syncPrepareResult, len(preps))
	results := make(chan syncResult, len(preps))
	if workerLimit == 0 {
		workerLimit = len(preps)
	}

	m := new(sync.Mutex)

	st.scatterGather(
		workerLimit,
		len(preps),
		func() {
			for i := 0; i < len(preps); i++ {
				jobQueue <- &preps[i]
			}
			close(jobQueue)
		},
		func(workerIndex int) {
			for prep := range jobQueue {
				release := prep.release
				flags := prep.flags
				chart := normalizeChart(st.basePath, release.Chart)
				var relErr *ReleaseError
				context := st.createHelmContext(release, workerIndex)

				if _, err := st.triggerPresyncEvent(release, "sync"); err != nil {
					relErr = newReleaseError(release, err)
				} else if !release.Desired() {
					installed, err := st.isReleaseInstalled(context, helm, *release)
					if err != nil {
						relErr = newReleaseError(release, err)
					} else if installed {
						var args []string
						if isHelm3() {
							args = []string{}
						} else {
							args = []string{"--purge"}
						}
						deletionFlags := st.appendConnectionFlags(args, release)
						m.Lock()
						if err := helm.DeleteRelease(context, release.Name, deletionFlags...); err != nil {
							affectedReleases.Failed = append(affectedReleases.Failed, release)
							relErr = newReleaseError(release, err)
						} else {
							affectedReleases.Deleted = append(affectedReleases.Deleted, release)
						}
						m.Unlock()
					}
				} else if err := helm.SyncRelease(context, release.Name, chart, flags...); err != nil {
					m.Lock()
					affectedReleases.Failed = append(affectedReleases.Failed, release)
					m.Unlock()
					relErr = newReleaseError(release, err)
				} else {
					m.Lock()
					affectedReleases.Upgraded = append(affectedReleases.Upgraded, release)
					m.Unlock()
					installedVersion, err := st.getDeployedVersion(context, helm, release)
					if err != nil { //err is not really impacting so just log it
						st.logger.Debugf("getting deployed release version failed:%v", err)
					} else {
						release.installedVersion = installedVersion
					}
				}

				if relErr == nil {
					results <- syncResult{}
				} else {
					results <- syncResult{errors: []*ReleaseError{relErr}}
				}

				if _, err := st.triggerPostsyncEvent(release, relErr, "sync"); err != nil {
					st.logger.Warnf("warn: %v\n", err)
				}

				if _, err := st.triggerCleanupEvent(release, "sync"); err != nil {
					st.logger.Warnf("warn: %v\n", err)
				}
			}
		},
		func() {
			for i := 0; i < len(preps); {
				select {
				case res := <-results:
					if len(res.errors) > 0 {
						for _, e := range res.errors {
							errs = append(errs, e)
						}
					}
				}
				i++
			}
		},
	)
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func (st *HelmState) listReleases(context helmexec.HelmContext, helm helmexec.Interface, release *ReleaseSpec) (string, error) {
	flags := st.connectionFlags(release)
	if isHelm3() && release.Namespace != "" {
		flags = append(flags, "--namespace", release.Namespace)
	}
	return helm.List(context, "^"+release.Name+"$", flags...)
}

func (st *HelmState) getDeployedVersion(context helmexec.HelmContext, helm helmexec.Interface, release *ReleaseSpec) (string, error) {
	//retrieve the version
	if out, err := st.listReleases(context, helm, release); err == nil {
		chartName := filepath.Base(release.Chart)
		//the regexp without escapes : .*\s.*\s.*\s.*\schartName-(.*?)\s
		pat := regexp.MustCompile(".*\\s.*\\s.*\\s.*\\s" + chartName + "-(.*?)\\s")
		versions := pat.FindStringSubmatch(out)
		if len(versions) > 0 {
			return versions[1], nil
		} else {
			//fails to find the version
			return "failed to get version", errors.New("Failed to get the version for:" + chartName)
		}
	} else {
		return "failed to get version", err
	}
}

// downloadCharts will download and untar charts for Lint and Template
func (st *HelmState) downloadCharts(helm helmexec.Interface, dir string, concurrency int, helmfileCommand string) (map[string]string, []error) {
	temp := make(map[string]string, len(st.Releases))
	type downloadResults struct {
		releaseName string
		chartPath   string
	}
	errs := []error{}

	jobQueue := make(chan *ReleaseSpec, len(st.Releases))
	results := make(chan *downloadResults, len(st.Releases))

	st.scatterGather(
		concurrency,
		len(st.Releases),
		func() {
			for i := 0; i < len(st.Releases); i++ {
				jobQueue <- &st.Releases[i]
			}
			close(jobQueue)
		},
		func(_ int) {
			for release := range jobQueue {
				chartPath := ""
				if pathExists(normalizeChart(st.basePath, release.Chart)) {
					chartPath = normalizeChart(st.basePath, release.Chart)
				} else {
					fetchFlags := []string{}
					if release.Version != "" {
						chartPath = path.Join(dir, release.Name, release.Version, release.Chart)
						fetchFlags = append(fetchFlags, "--version", release.Version)
					} else {
						chartPath = path.Join(dir, release.Name, "latest", release.Chart)
					}

					if st.isDevelopment(release) {
						fetchFlags = append(fetchFlags, "--devel")
					}

					// only fetch chart if it is not already fetched
					if _, err := os.Stat(chartPath); os.IsNotExist(err) {
						fetchFlags = append(fetchFlags, "--untar", "--untardir", chartPath)
						if err := helm.Fetch(release.Chart, fetchFlags...); err != nil {
							errs = append(errs, err)
						}
					}
					// Set chartPath to be the path containing Chart.yaml, if found
					fullChartPath, err := findChartDirectory(chartPath)
					if err == nil {
						chartPath = filepath.Dir(fullChartPath)
					}
				}
				results <- &downloadResults{release.Name, chartPath}
			}
		},
		func() {
			for i := 0; i < len(st.Releases); i++ {
				downloadRes := <-results
				temp[downloadRes.releaseName] = downloadRes.chartPath
			}
		},
	)

	if len(errs) > 0 {
		return nil, errs
	}
	return temp, nil
}

type TemplateOpts struct {
	Set []string
}

type TemplateOpt interface{ Apply(*TemplateOpts) }

func (o *TemplateOpts) Apply(opts *TemplateOpts) {
	*opts = *o
}

// TemplateReleases wrapper for executing helm template on the releases
func (st *HelmState) TemplateReleases(helm helmexec.Interface, outputDir string, additionalValues []string, args []string, workerLimit int, opt ...TemplateOpt) []error {
	opts := &TemplateOpts{}
	for _, o := range opt {
		o.Apply(opts)
	}

	// Reset the extra args if already set, not to break `helm fetch` by adding the args intended for `lint`
	helm.SetExtraArgs()

	errs := []error{}
	// Create tmp directory and bail immediately if it fails
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		errs = append(errs, err)
		return errs
	}
	defer os.RemoveAll(dir)

	temp, errs := st.downloadCharts(helm, dir, workerLimit, "template")

	if errs != nil {
		errs = append(errs, err)
		return errs
	}

	if len(args) > 0 {
		helm.SetExtraArgs(args...)
	}

	for i := range st.Releases {
		release := st.Releases[i]

		if !release.Desired() {
			continue
		}

		st.ApplyOverrides(&release)

		flags, err := st.flagsForTemplate(helm, &release, 0)
		if err != nil {
			errs = append(errs, err)
		}

		for _, value := range additionalValues {
			valfile, err := filepath.Abs(value)
			if err != nil {
				errs = append(errs, err)
			}

			if _, err := os.Stat(valfile); os.IsNotExist(err) {
				errs = append(errs, err)
			}
			flags = append(flags, "--values", valfile)
		}

		if opts.Set != nil {
			for _, s := range opts.Set {
				flags = append(flags, "--set", s)
			}
		}

		if len(outputDir) > 0 {
			releaseOutputDir, err := st.GenerateOutputDir(outputDir, release)
			if err != nil {
				errs = append(errs, err)
			}

			flags = append(flags, "--output-dir", releaseOutputDir)
			st.logger.Debugf("Generating templates to : %s\n", releaseOutputDir)
			os.Mkdir(releaseOutputDir, 0755)
		}

		if len(errs) == 0 {
			if err := helm.TemplateRelease(release.Name, temp[release.Name], flags...); err != nil {
				errs = append(errs, err)
			}
		}

		if _, err := st.triggerCleanupEvent(&release, "template"); err != nil {
			st.logger.Warnf("warn: %v\n", err)
		}
	}

	if len(errs) != 0 {
		return errs
	}

	return nil
}

type LintOpts struct {
	Set []string
}

type LintOpt interface{ Apply(*LintOpts) }

func (o *LintOpts) Apply(opts *LintOpts) {
	*opts = *o
}

// LintReleases wrapper for executing helm lint on the releases
func (st *HelmState) LintReleases(helm helmexec.Interface, additionalValues []string, args []string, workerLimit int, opt ...LintOpt) []error {
	opts := &LintOpts{}
	for _, o := range opt {
		o.Apply(opts)
	}

	// Reset the extra args if already set, not to break `helm fetch` by adding the args intended for `lint`
	helm.SetExtraArgs()

	errs := []error{}
	// Create tmp directory and bail immediately if it fails
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		errs = append(errs, err)
		return errs
	}
	defer os.RemoveAll(dir)

	temp, errs := st.downloadCharts(helm, dir, workerLimit, "lint")
	if errs != nil {
		errs = append(errs, err)
		return errs
	}

	if len(args) > 0 {
		helm.SetExtraArgs(args...)
	}

	for i := range st.Releases {
		release := st.Releases[i]

		if !release.Desired() {
			continue
		}

		flags, err := st.flagsForLint(helm, &release, 0)
		if err != nil {
			errs = append(errs, err)
		}
		for _, value := range additionalValues {
			valfile, err := filepath.Abs(value)
			if err != nil {
				errs = append(errs, err)
			}

			if _, err := os.Stat(valfile); os.IsNotExist(err) {
				errs = append(errs, err)
			}
			flags = append(flags, "--values", valfile)
		}

		if opts.Set != nil {
			for _, s := range opts.Set {
				flags = append(flags, "--set", s)
			}
		}

		if len(errs) == 0 {
			if err := helm.Lint(release.Name, temp[release.Name], flags...); err != nil {
				errs = append(errs, err)
			}
		}

		if _, err := st.triggerCleanupEvent(&release, "lint"); err != nil {
			st.logger.Warnf("warn: %v\n", err)
		}
	}

	if len(errs) != 0 {
		return errs
	}

	return nil
}

type diffResult struct {
	err *ReleaseError
}

type diffPrepareResult struct {
	release *ReleaseSpec
	flags   []string
	errors  []*ReleaseError
}

func (st *HelmState) prepareDiffReleases(helm helmexec.Interface, additionalValues []string, concurrency int, detailedExitCode, suppressSecrets bool, opt ...DiffOpt) ([]diffPrepareResult, []error) {
	opts := &DiffOpts{}
	for _, o := range opt {
		o.Apply(opts)
	}

	releases := []*ReleaseSpec{}
	for i, _ := range st.Releases {
		if !st.Releases[i].Desired() {
			continue
		}
		releases = append(releases, &st.Releases[i])
	}

	numReleases := len(releases)
	jobs := make(chan *ReleaseSpec, numReleases)
	results := make(chan diffPrepareResult, numReleases)

	rs := []diffPrepareResult{}
	errs := []error{}

	mut := sync.Mutex{}

	st.scatterGather(
		concurrency,
		numReleases,
		func() {
			for i := 0; i < numReleases; i++ {
				jobs <- releases[i]
			}
			close(jobs)
		},
		func(workerIndex int) {
			for release := range jobs {
				errs := []error{}

				st.ApplyOverrides(release)

				// TODO We need a long-term fix for this :)
				// See https://github.com/roboll/helmfile/issues/737
				mut.Lock()
				flags, err := st.flagsForDiff(helm, release, workerIndex)
				mut.Unlock()
				if err != nil {
					errs = append(errs, err)
				}

				for _, value := range additionalValues {
					valfile, err := filepath.Abs(value)
					if err != nil {
						errs = append(errs, err)
					}

					if _, err := os.Stat(valfile); os.IsNotExist(err) {
						errs = append(errs, err)
					}
					flags = append(flags, "--values", valfile)
				}

				if detailedExitCode {
					flags = append(flags, "--detailed-exitcode")
				}

				if suppressSecrets {
					flags = append(flags, "--suppress-secrets")
				}

				if opts.NoColor {
					flags = append(flags, "--no-color")
				}

				if opts.Context > 0 {
					flags = append(flags, "--context", fmt.Sprintf("%d", opts.Context))
				}

				if opts.Set != nil {
					for _, s := range opts.Set {
						flags = append(flags, "--set", s)
					}
				}

				if len(errs) > 0 {
					rsErrs := make([]*ReleaseError, len(errs))
					for i, e := range errs {
						rsErrs[i] = newReleaseError(release, e)
					}
					results <- diffPrepareResult{errors: rsErrs}
				} else {
					results <- diffPrepareResult{release: release, flags: flags, errors: []*ReleaseError{}}
				}
			}
		},
		func() {
			for i := 0; i < numReleases; i++ {
				res := <-results
				if res.errors != nil && len(res.errors) > 0 {
					for _, e := range res.errors {
						errs = append(errs, e)
					}
				} else if res.release != nil {
					rs = append(rs, res)
				}
			}
		},
	)

	return rs, errs
}

func (st *HelmState) createHelmContext(spec *ReleaseSpec, workerIndex int) helmexec.HelmContext {
	namespace := st.HelmDefaults.TillerNamespace
	if spec.TillerNamespace != "" {
		namespace = spec.TillerNamespace
	}
	tillerless := st.HelmDefaults.Tillerless
	if spec.Tillerless != nil {
		tillerless = *spec.Tillerless
	}

	return helmexec.HelmContext{
		Tillerless:      tillerless,
		TillerNamespace: namespace,
		WorkerIndex:     workerIndex,
	}
}

type DiffOpts struct {
	Context int
	NoColor bool
	Set     []string
}

func (o *DiffOpts) Apply(opts *DiffOpts) {
	*opts = *o
}

type DiffOpt interface{ Apply(*DiffOpts) }

// DiffReleases wrapper for executing helm diff on the releases
// It returns releases that had any changes
func (st *HelmState) DiffReleases(helm helmexec.Interface, additionalValues []string, workerLimit int, detailedExitCode, suppressSecrets bool, triggerCleanupEvents bool, opt ...DiffOpt) ([]ReleaseSpec, []error) {
	opts := &DiffOpts{}
	for _, o := range opt {
		o.Apply(opts)
	}

	preps, prepErrs := st.prepareDiffReleases(helm, additionalValues, workerLimit, detailedExitCode, suppressSecrets, opts)
	if len(prepErrs) > 0 {
		return []ReleaseSpec{}, prepErrs
	}

	jobQueue := make(chan *diffPrepareResult, len(preps))
	results := make(chan diffResult, len(preps))

	rs := []ReleaseSpec{}
	errs := []error{}

	st.scatterGather(
		workerLimit,
		len(preps),
		func() {
			for i := 0; i < len(preps); i++ {
				jobQueue <- &preps[i]
			}
			close(jobQueue)
		},
		func(workerIndex int) {
			for prep := range jobQueue {
				flags := prep.flags
				release := prep.release
				if err := helm.DiffRelease(st.createHelmContext(release, workerIndex), release.Name, normalizeChart(st.basePath, release.Chart), flags...); err != nil {
					switch e := err.(type) {
					case helmexec.ExitError:
						// Propagate any non-zero exit status from the external command like `helm` that is failed under the hood
						results <- diffResult{&ReleaseError{release, err, e.ExitStatus()}}
					default:
						results <- diffResult{&ReleaseError{release, err, 0}}
					}
				} else {
					// diff succeeded, found no changes
					results <- diffResult{}
				}

				if triggerCleanupEvents {
					if _, err := st.triggerCleanupEvent(prep.release, "diff"); err != nil {
						st.logger.Warnf("warn: %v\n", err)
					}
				}
			}
		},
		func() {
			for i := 0; i < len(preps); i++ {
				res := <-results
				if res.err != nil {
					errs = append(errs, res.err)
					if res.err.Code == 2 {
						rs = append(rs, *res.err.ReleaseSpec)
					}
				}
			}
		},
	)

	return rs, errs
}

func (st *HelmState) ReleaseStatuses(helm helmexec.Interface, workerLimit int) []error {
	return st.scatterGatherReleases(helm, workerLimit, func(release ReleaseSpec, workerIndex int) error {
		if !release.Desired() {
			return nil
		}

		flags := []string{}
		flags = st.appendConnectionFlags(flags, &release)

		return helm.ReleaseStatus(st.createHelmContext(&release, workerIndex), release.Name, flags...)
	})
}

// DeleteReleases wrapper for executing helm delete on the releases
// This function traverses the DAG of the releases in the reverse order, so that the releases that are NOT depended by any others are deleted first.
func (st *HelmState) DeleteReleases(affectedReleases *AffectedReleases, helm helmexec.Interface, concurrency int, purge bool) []error {
	return st.scatterGatherReleases(helm, concurrency, func(release ReleaseSpec, workerIndex int) error {
		if !release.Desired() {
			return nil
		}

		st.ApplyOverrides(&release)

		flags := []string{}
		if purge && !isHelm3() {
			flags = append(flags, "--purge")
		}
		flags = st.appendConnectionFlags(flags, &release)
		if isHelm3() && release.Namespace != "" {
			flags = append(flags, "--namespace", release.Namespace)
		}
		context := st.createHelmContext(&release, workerIndex)

		installed, err := st.isReleaseInstalled(context, helm, release)
		if err != nil {
			return err
		}
		if installed {
			if err := helm.DeleteRelease(context, release.Name, flags...); err != nil {
				affectedReleases.Failed = append(affectedReleases.Failed, &release)
				return err
			} else {
				affectedReleases.Deleted = append(affectedReleases.Deleted, &release)
				return nil
			}
		}
		return nil
	})
}

// TestReleases wrapper for executing helm test on the releases
func (st *HelmState) TestReleases(helm helmexec.Interface, cleanup bool, timeout int, concurrency int) []error {
	return st.scatterGatherReleases(helm, concurrency, func(release ReleaseSpec, workerIndex int) error {
		if !release.Desired() {
			return nil
		}

		flags := []string{}
		if cleanup {
			flags = append(flags, "--cleanup")
		}
		duration := strconv.Itoa(timeout)
		if isHelm3() {
			duration += "s"
		}
		flags = append(flags, "--timeout", duration)
		flags = st.appendConnectionFlags(flags, &release)

		return helm.TestRelease(st.createHelmContext(&release, workerIndex), release.Name, flags...)
	})
}

func isHelm3() bool {
	return os.Getenv("HELMFILE_HELM3") != ""
}

// Clean will remove any generated secrets
func (st *HelmState) Clean() []error {
	errs := []error{}

	for _, release := range st.Releases {
		for _, value := range release.generatedValues {
			err := st.removeFile(value)
			if err != nil {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) != 0 {
		return errs
	}

	return nil
}

func MarkFilteredReleases(releases []ReleaseSpec, selectors []string) ([]Release, error) {
	var filteredReleases []Release
	filters := []ReleaseFilter{}
	for _, label := range selectors {
		f, err := ParseLabels(label)
		if err != nil {
			return nil, err
		}
		filters = append(filters, f)
	}
	for _, r := range releases {
		if r.Labels == nil {
			r.Labels = map[string]string{}
		}
		// Let the release name, namespace, and chart be used as a tag
		r.Labels["name"] = r.Name
		r.Labels["namespace"] = r.Namespace
		// Strip off just the last portion for the name stable/newrelic would give newrelic
		chartSplit := strings.Split(r.Chart, "/")
		r.Labels["chart"] = chartSplit[len(chartSplit)-1]
		var matched bool
		for _, f := range filters {
			if r.Labels == nil {
				r.Labels = map[string]string{}
			}
			if f.Match(r) {
				matched = true
				break
			}
		}
		res := Release{
			ReleaseSpec: r,
			Filtered:    len(filters) > 0 && !matched,
		}
		filteredReleases = append(filteredReleases, res)
	}

	return filteredReleases, nil
}

func (st *HelmState) GetFilteredReleases() ([]ReleaseSpec, error) {
	filteredReleases, err := MarkFilteredReleases(st.Releases, st.Selectors)
	if err != nil {
		return nil, err
	}
	var releases []ReleaseSpec
	for _, r := range filteredReleases {
		if !r.Filtered {
			releases = append(releases, r.ReleaseSpec)
		}
	}
	return releases, nil
}

// FilterReleases allows for the execution of helm commands against a subset of the releases in the helmfile.
func (st *HelmState) FilterReleases() error {
	releases, err := st.GetFilteredReleases()
	if err != nil {
		return err
	}
	st.Releases = releases
	st.logger.Debugf("%d release(s) matching %s found in %s\n", len(releases), strings.Join(st.Selectors, ","), st.FilePath)
	return nil
}

func (st *HelmState) PrepareReleases(helm helmexec.Interface, helmfileCommand string) []error {
	errs := []error{}

	for i := range st.Releases {
		release := st.Releases[i]

		if _, err := st.triggerPrepareEvent(&release, helmfileCommand); err != nil {
			errs = append(errs, newReleaseError(&release, err))
			continue
		}
	}
	if len(errs) != 0 {
		return errs
	}

	updated, err := st.ResolveDeps()
	if err != nil {
		return []error{err}
	}

	*st = *updated

	return nil
}

func (st *HelmState) triggerPrepareEvent(r *ReleaseSpec, helmfileCommand string) (bool, error) {
	return st.triggerReleaseEvent("prepare", nil, r, helmfileCommand)
}

func (st *HelmState) triggerCleanupEvent(r *ReleaseSpec, helmfileCommand string) (bool, error) {
	return st.triggerReleaseEvent("cleanup", nil, r, helmfileCommand)
}

func (st *HelmState) triggerPresyncEvent(r *ReleaseSpec, helmfileCommand string) (bool, error) {
	return st.triggerReleaseEvent("presync", nil, r, helmfileCommand)
}

func (st *HelmState) triggerPostsyncEvent(r *ReleaseSpec, evtErr error, helmfileCommand string) (bool, error) {
	return st.triggerReleaseEvent("postsync", evtErr, r, helmfileCommand)
}

func (st *HelmState) triggerReleaseEvent(evt string, evtErr error, r *ReleaseSpec, helmfileCmd string) (bool, error) {
	bus := &event.Bus{
		Hooks:         r.Hooks,
		StateFilePath: st.FilePath,
		BasePath:      st.basePath,
		Namespace:     st.OverrideNamespace,
		Env:           st.Env,
		Logger:        st.logger,
		ReadFile:      st.readFile,
	}
	data := map[string]interface{}{
		"Release":         r,
		"HelmfileCommand": helmfileCmd,
	}
	return bus.Trigger(evt, evtErr, data)
}

// ResolveDeps returns a copy of this helmfile state with the concrete chart version numbers filled in for remote chart dependencies
func (st *HelmState) ResolveDeps() (*HelmState, error) {
	return st.mergeLockedDependencies()
}

// UpdateDeps wrapper for updating dependencies on the releases
func (st *HelmState) UpdateDeps(helm helmexec.Interface) []error {
	var errs []error

	for _, release := range st.Releases {
		if isLocalChart(release.Chart) {
			if err := helm.UpdateDeps(normalizeChart(st.basePath, release.Chart)); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) == 0 {
		tempDir := st.tempDir
		if tempDir == nil {
			tempDir = ioutil.TempDir
		}
		_, err := st.updateDependenciesInTempDir(helm, tempDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("unable to update deps: %v", err))
		}
	}

	if len(errs) != 0 {
		return errs
	}
	return nil
}

// BuildDeps wrapper for building dependencies on the releases
func (st *HelmState) BuildDeps(helm helmexec.Interface) []error {
	errs := []error{}

	for _, release := range st.Releases {
		if isLocalChart(release.Chart) {
			if err := helm.BuildDeps(release.Name, normalizeChart(st.basePath, release.Chart)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) != 0 {
		return errs
	}
	return nil
}

func pathExists(chart string) bool {
	_, err := os.Stat(chart)
	return err == nil
}

func chartNameWithoutRepository(chart string) string {
	chartSplit := strings.Split(chart, "/")
	return chartSplit[len(chartSplit)-1]
}

// find "Chart.yaml"
func findChartDirectory(topLevelDir string) (string, error) {
	var files []string
	filepath.Walk(topLevelDir, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("error walking through %s: %v", path, err)
		}
		if !f.IsDir() {
			r, err := regexp.MatchString("Chart.yaml", f.Name())
			if err == nil && r {
				files = append(files, path)
			}
		}
		return nil
	})
	// Sort to get the shortest path
	sort.Strings(files)
	if len(files) > 0 {
		first := files[0]
		return first, nil
	}

	return topLevelDir, errors.New("No Chart.yaml found")
}

// appendConnectionFlags append all the helm command-line flags related to K8s API and Tiller connection including the kubecontext
func (st *HelmState) appendConnectionFlags(flags []string, release *ReleaseSpec) []string {
	adds := st.connectionFlags(release)
	for _, a := range adds {
		flags = append(flags, a)
	}
	return flags
}

func (st *HelmState) connectionFlags(release *ReleaseSpec) []string {
	flags := []string{}
	tillerless := st.HelmDefaults.Tillerless
	if release.Tillerless != nil {
		tillerless = *release.Tillerless
	}
	if !tillerless {
		if release.TillerNamespace != "" {
			flags = append(flags, "--tiller-namespace", release.TillerNamespace)
		} else if st.HelmDefaults.TillerNamespace != "" {
			flags = append(flags, "--tiller-namespace", st.HelmDefaults.TillerNamespace)
		}

		if release.TLS != nil && *release.TLS || release.TLS == nil && st.HelmDefaults.TLS {
			flags = append(flags, "--tls")
		}

		if release.TLSKey != "" {
			flags = append(flags, "--tls-key", release.TLSKey)
		} else if st.HelmDefaults.TLSKey != "" {
			flags = append(flags, "--tls-key", st.HelmDefaults.TLSKey)
		}

		if release.TLSCert != "" {
			flags = append(flags, "--tls-cert", release.TLSCert)
		} else if st.HelmDefaults.TLSCert != "" {
			flags = append(flags, "--tls-cert", st.HelmDefaults.TLSCert)
		}

		if release.TLSCACert != "" {
			flags = append(flags, "--tls-ca-cert", release.TLSCACert)
		} else if st.HelmDefaults.TLSCACert != "" {
			flags = append(flags, "--tls-ca-cert", st.HelmDefaults.TLSCACert)
		}

		if release.KubeContext != "" {
			flags = append(flags, "--kube-context", release.KubeContext)
		} else if st.HelmDefaults.KubeContext != "" {
			flags = append(flags, "--kube-context", st.HelmDefaults.KubeContext)
		}
	}

	return flags
}

func (st *HelmState) flagsForUpgrade(helm helmexec.Interface, release *ReleaseSpec, workerIndex int) ([]string, error) {
	flags := []string{}
	if release.Version != "" {
		flags = append(flags, "--version", release.Version)
	}

	if st.isDevelopment(release) {
		flags = append(flags, "--devel")
	}

	if release.Verify != nil && *release.Verify || release.Verify == nil && st.HelmDefaults.Verify {
		flags = append(flags, "--verify")
	}

	if release.Wait != nil && *release.Wait || release.Wait == nil && st.HelmDefaults.Wait {
		flags = append(flags, "--wait")
	}

	timeout := st.HelmDefaults.Timeout
	if release.Timeout != nil {
		timeout = *release.Timeout
	}
	if timeout != 0 {
		duration := strconv.Itoa(timeout)
		if isHelm3() {
			duration += "s"
		}
		flags = append(flags, "--timeout", duration)
	}

	if release.Force != nil && *release.Force || release.Force == nil && st.HelmDefaults.Force {
		flags = append(flags, "--force")
	}

	if release.RecreatePods != nil && *release.RecreatePods || release.RecreatePods == nil && st.HelmDefaults.RecreatePods {
		flags = append(flags, "--recreate-pods")
	}

	if release.Atomic != nil && *release.Atomic || release.Atomic == nil && st.HelmDefaults.Atomic {
		flags = append(flags, "--atomic")
	}

	flags = st.appendConnectionFlags(flags, release)

	var err error
	flags, err = st.appendHelmXFlags(flags, release)
	if err != nil {
		return nil, err
	}

	common, err := st.namespaceAndValuesFlags(helm, release, workerIndex)
	if err != nil {
		return nil, err
	}
	return append(flags, common...), nil
}

func (st *HelmState) flagsForTemplate(helm helmexec.Interface, release *ReleaseSpec, workerIndex int) ([]string, error) {
	flags := []string{}

	var err error
	flags, err = st.appendHelmXFlags(flags, release)
	if err != nil {
		return nil, err
	}

	common, err := st.namespaceAndValuesFlags(helm, release, workerIndex)
	if err != nil {
		return nil, err
	}
	return append(flags, common...), nil
}

func (st *HelmState) flagsForDiff(helm helmexec.Interface, release *ReleaseSpec, workerIndex int) ([]string, error) {
	flags := []string{}
	if release.Version != "" {
		flags = append(flags, "--version", release.Version)
	}

	if st.isDevelopment(release) {
		flags = append(flags, "--devel")
	}

	flags = st.appendConnectionFlags(flags, release)

	var err error
	flags, err = st.appendHelmXFlags(flags, release)
	if err != nil {
		return nil, err
	}

	common, err := st.namespaceAndValuesFlags(helm, release, workerIndex)
	if err != nil {
		return nil, err
	}
	return append(flags, common...), nil
}

func (st *HelmState) isDevelopment(release *ReleaseSpec) bool {
	result := st.HelmDefaults.Devel
	if release.Devel != nil {
		result = *release.Devel
	}

	return result
}

func (st *HelmState) flagsForLint(helm helmexec.Interface, release *ReleaseSpec, workerIndex int) ([]string, error) {
	flags, err := st.namespaceAndValuesFlags(helm, release, workerIndex)
	if err != nil {
		return nil, err
	}

	flags, err = st.appendHelmXFlags(flags, release)
	if err != nil {
		return nil, err
	}

	return flags, nil
}

func (st *HelmState) RenderValuesFileToBytes(path string) ([]byte, error) {
	r := tmpl.NewFileRenderer(st.readFile, filepath.Dir(path), st.valuesFileTemplateData())
	rawBytes, err := r.RenderToBytes(path)
	if err != nil {
		return nil, err
	}

	// If 'ref+.*' exists in file, run vals against the file
	match, err := regexp.Match("ref\\+.*", rawBytes)
	if err != nil {
		return nil, err
	}

	if match {
		var rawYaml map[string]interface{}

		if err := yaml.Unmarshal(rawBytes, &rawYaml); err != nil {
			return nil, err
		}

		parsedYaml, err := st.valsRuntime.Eval(rawYaml)
		if err != nil {
			return nil, err
		}

		return yaml.Marshal(parsedYaml)
	}

	return rawBytes, nil
}

func (st *HelmState) storage() *Storage {
	return &Storage{
		FilePath: st.FilePath,
		basePath: st.basePath,
		glob:     st.glob,
		logger:   st.logger,
	}
}

func (st *HelmState) ExpandedHelmfiles() ([]SubHelmfileSpec, error) {
	helmfiles := []SubHelmfileSpec{}
	for _, hf := range st.Helmfiles {
		if remote.IsRemote(hf.Path) {
			helmfiles = append(helmfiles, hf)
			continue
		}

		matches, err := st.storage().ExpandPaths(hf.Path)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			continue
		}
		for _, match := range matches {
			newHelmfile := hf
			newHelmfile.Path = match
			helmfiles = append(helmfiles, newHelmfile)
		}
	}

	return helmfiles, nil
}

func (st *HelmState) generateTemporaryValuesFiles(values []interface{}, missingFileHandler *string) ([]string, error) {
	generatedFiles := []string{}

	for _, value := range values {
		switch typedValue := value.(type) {
		case string:
			paths, skip, err := st.storage().resolveFile(missingFileHandler, "values", typedValue)
			if err != nil {
				return nil, err
			}
			if skip {
				continue
			}

			if len(paths) > 1 {
				return nil, fmt.Errorf("glob patterns in release values and secrets is not supported yet. please submit a feature request if necessary")
			}
			path := paths[0]

			yamlBytes, err := st.RenderValuesFileToBytes(path)
			if err != nil {
				return nil, fmt.Errorf("failed to render values files \"%s\": %v", typedValue, err)
			}

			valfile, err := ioutil.TempFile("", "values")
			if err != nil {
				return nil, err
			}
			defer valfile.Close()

			if _, err := valfile.Write(yamlBytes); err != nil {
				return nil, fmt.Errorf("failed to write %s: %v", valfile.Name(), err)
			}
			st.logger.Debugf("successfully generated the value file at %s. produced:\n%s", path, string(yamlBytes))
			generatedFiles = append(generatedFiles, valfile.Name())
		case map[interface{}]interface{}, map[string]interface{}:
			valfile, err := ioutil.TempFile("", "values")
			if err != nil {
				return nil, err
			}
			defer valfile.Close()
			encoder := yaml.NewEncoder(valfile)
			defer encoder.Close()
			if err := encoder.Encode(typedValue); err != nil {
				return nil, err
			}
			generatedFiles = append(generatedFiles, valfile.Name())
		default:
			return nil, fmt.Errorf("unexpected type of value: value=%v, type=%T", typedValue, typedValue)
		}
	}
	return generatedFiles, nil
}

func (st *HelmState) namespaceAndValuesFlags(helm helmexec.Interface, release *ReleaseSpec, workerIndex int) ([]string, error) {
	flags := []string{}
	if release.Namespace != "" {
		flags = append(flags, "--namespace", release.Namespace)
	}

	values := []interface{}{}
	for _, v := range release.Values {
		switch typedValue := v.(type) {
		case string:
			path := st.storage().normalizePath(release.ValuesPathPrefix + typedValue)
			values = append(values, path)
		default:
			values = append(values, v)
		}
	}

	valuesMapSecretsRendered, err := st.valsRuntime.Eval(map[string]interface{}{"values": values})
	if err != nil {
		return nil, err
	}

	valuesSecretsRendered, ok := valuesMapSecretsRendered["values"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("Failed to render values in %s for release %s: type %T isn't supported", st.FilePath, release.Name, valuesMapSecretsRendered["values"])
	}

	generatedFiles, err := st.generateTemporaryValuesFiles(valuesSecretsRendered, release.MissingFileHandler)
	if err != nil {
		return nil, err
	}

	for _, f := range generatedFiles {
		flags = append(flags, "--values", f)
	}

	release.generatedValues = append(release.generatedValues, generatedFiles...)

	for _, value := range release.Secrets {
		paths, skip, err := st.storage().resolveFile(release.MissingFileHandler, "secrets", release.ValuesPathPrefix+value)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}

		if len(paths) > 1 {
			return nil, fmt.Errorf("glob patterns in release secret file is not supported yet. please submit a feature request if necessary")
		}
		path := paths[0]

		decryptFlags := st.appendConnectionFlags([]string{}, release)
		valfile, err := helm.DecryptSecret(st.createHelmContext(release, workerIndex), path, decryptFlags...)
		if err != nil {
			return nil, err
		}

		release.generatedValues = append(release.generatedValues, valfile)
		flags = append(flags, "--values", valfile)
	}
	if len(release.SetValues) > 0 {
		for _, set := range release.SetValues {
			if set.Value != "" {
				renderedValue, err := renderValsSecrets(st.valsRuntime, set.Value)
				if err != nil {
					return nil, fmt.Errorf("Failed to render set value entry in %s for release %s: %v", st.FilePath, release.Name, err)
				}
				flags = append(flags, "--set", fmt.Sprintf("%s=%s", escape(set.Name), escape(renderedValue[0])))
			} else if set.File != "" {
				flags = append(flags, "--set-file", fmt.Sprintf("%s=%s", escape(set.Name), st.storage().normalizePath(set.File)))
			} else if len(set.Values) > 0 {
				renderedValues, err := renderValsSecrets(st.valsRuntime, set.Values...)
				if err != nil {
					return nil, fmt.Errorf("Failed to render set values entry in %s for release %s: %v", st.FilePath, release.Name, err)
				}
				items := make([]string, len(renderedValues))
				for i, raw := range renderedValues {
					items[i] = escape(raw)
				}
				v := strings.Join(items, ",")
				flags = append(flags, "--set", fmt.Sprintf("%s={%s}", escape(set.Name), v))
			}
		}
	}

	/***********
	 * START 'env' section for backwards compatibility
	 ***********/
	// The 'env' section is not really necessary any longer, as 'set' would now provide the same functionality
	if len(release.EnvValues) > 0 {
		val := []string{}
		envValErrs := []string{}
		for _, set := range release.EnvValues {
			value, isSet := os.LookupEnv(set.Value)
			if isSet {
				val = append(val, fmt.Sprintf("%s=%s", escape(set.Name), escape(value)))
			} else {
				errMsg := fmt.Sprintf("\t%s", set.Value)
				envValErrs = append(envValErrs, errMsg)
			}
		}
		if len(envValErrs) != 0 {
			joinedEnvVals := strings.Join(envValErrs, "\n")
			errMsg := fmt.Sprintf("Environment Variables not found. Please make sure they are set and try again:\n%s", joinedEnvVals)
			return nil, errors.New(errMsg)
		}
		flags = append(flags, "--set", strings.Join(val, ","))
	}
	/**************
	 * END 'env' section for backwards compatibility
	 **************/

	return flags, nil
}

// renderValsSecrets helper function which renders 'ref+.*' secrets
func renderValsSecrets(e vals.Evaluator, input ...string) ([]string, error) {
	output := make([]string, len(input))
	if len(input) > 0 {
		mapRendered, err := e.Eval(map[string]interface{}{"values": input})
		if err != nil {
			return nil, err
		}

		rendered, ok := mapRendered["values"].([]interface{})
		if !ok {
			return nil, fmt.Errorf("type %T isn't supported", mapRendered["values"])
		}

		for i := 0; i < len(rendered); i++ {
			output[i] = fmt.Sprintf("%v", rendered[i])
		}
	}
	return output, nil
}

// DisplayAffectedReleases logs the upgraded, deleted and in error releases
func (ar *AffectedReleases) DisplayAffectedReleases(logger *zap.SugaredLogger) {
	if ar.Upgraded != nil && len(ar.Upgraded) > 0 {
		logger.Info("\nUPDATED RELEASES:")
		tbl, _ := prettytable.NewTable(prettytable.Column{Header: "NAME"},
			prettytable.Column{Header: "CHART", MinWidth: 6},
			prettytable.Column{Header: "VERSION", AlignRight: true},
		)
		tbl.Separator = "   "
		for _, release := range ar.Upgraded {
			tbl.AddRow(release.Name, release.Chart, release.installedVersion)
		}
		logger.Info(tbl.String())
	}
	if ar.Deleted != nil && len(ar.Deleted) > 0 {
		logger.Info("\nDELETED RELEASES:")
		logger.Info("NAME")
		for _, release := range ar.Deleted {
			logger.Info(release.Name)
		}
	}
	if ar.Failed != nil && len(ar.Failed) > 0 {
		logger.Info("\nFAILED RELEASES:")
		logger.Info("NAME")
		for _, release := range ar.Failed {
			logger.Info(release.Name)
		}
	}
}

func escape(value string) string {
	intermediate := strings.Replace(value, "{", "\\{", -1)
	intermediate = strings.Replace(intermediate, "}", "\\}", -1)
	return strings.Replace(intermediate, ",", "\\,", -1)
}

//UnmarshalYAML will unmarshal the helmfile yaml section and fill the SubHelmfileSpec structure
//this is required to keep allowing string scalar for defining helmfile
func (hf *SubHelmfileSpec) UnmarshalYAML(unmarshal func(interface{}) error) error {

	var tmp interface{}
	if err := unmarshal(&tmp); err != nil {
		return err
	}

	switch i := tmp.(type) {
	case string: // single path definition without sub items, legacy sub helmfile definition
		hf.Path = i
	case map[interface{}]interface{}: // helmfile path with sub section
		var subHelmfileSpecTmp struct {
			Path               string   `yaml:"path"`
			Selectors          []string `yaml:"selectors"`
			SelectorsInherited bool     `yaml:"selectorsInherited"`

			Environment SubhelmfileEnvironmentSpec `yaml:",inline"`
		}
		if err := unmarshal(&subHelmfileSpecTmp); err != nil {
			return err
		}
		hf.Path = subHelmfileSpecTmp.Path
		hf.Selectors = subHelmfileSpecTmp.Selectors
		hf.SelectorsInherited = subHelmfileSpecTmp.SelectorsInherited
		hf.Environment = subHelmfileSpecTmp.Environment
	}
	//since we cannot make sur the "console" string can be red after the "path" we must check we don't have
	//a SubHelmfileSpec with only selector and no path
	if hf.Selectors != nil && hf.Path == "" {
		return fmt.Errorf("found 'selectors' definition without path: %v", hf.Selectors)
	}
	//also exclude SelectorsInherited to true and explicit selectors
	if hf.SelectorsInherited && len(hf.Selectors) > 0 {
		return fmt.Errorf("You cannot use 'SelectorsInherited: true' along with and explicit selector for path: %v", hf.Path)
	}
	return nil
}

func (st *HelmState) GenerateOutputDir(outputDir string, release ReleaseSpec) (string, error) {
	// get absolute path of state file to generate a hash
	// use this hash to write helm output in a specific directory by state file and release name
	// ie. in a directory named stateFileName-stateFileHash-releaseName
	stateAbsPath, err := filepath.Abs(st.FilePath)
	if err != nil {
		return stateAbsPath, err
	}

	hasher := sha1.New()
	io.WriteString(hasher, stateAbsPath)

	var stateFileExtension = filepath.Ext(st.FilePath)
	var stateFileName = st.FilePath[0 : len(st.FilePath)-len(stateFileExtension)]

	var sb strings.Builder
	sb.WriteString(stateFileName)
	sb.WriteString("-")
	sb.WriteString(hex.EncodeToString(hasher.Sum(nil))[:8])
	sb.WriteString("-")
	sb.WriteString(release.Name)

	return path.Join(outputDir, sb.String()), nil
}

func (st *HelmState) ToYaml() (string, error) {
	if result, err := yaml.Marshal(st); err != nil {
		return "", err
	} else {
		return string(result), nil
	}
}
