package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/openshift/sippy/pkg/buganalysis"
	"github.com/openshift/sippy/pkg/sippyserver"
	"github.com/openshift/sippy/pkg/testgridanalysis/testgridconversion"
	"github.com/openshift/sippy/pkg/testgridanalysis/testgridhelpers"
	"github.com/openshift/sippy/pkg/testgridanalysis/testidentification"
	"github.com/openshift/sippy/pkg/util/sets"
	"github.com/spf13/cobra"
	"k8s.io/klog"
)

type Options struct {
	LocalData         string
	OpenshiftReleases []string
	Dashboards        []string
	// TODO perhaps this could drive the synthetic tests too
	Variants                []string
	StartDay                int
	endDay                  int
	NumDays                 int
	TestSuccessThreshold    float64
	JobFilter               string
	MinTestRuns             int
	Output                  string
	FailureClusterThreshold int
	FetchData               string
	ListenAddr              string
	Server                  bool
	SkipBugLookup           bool
}

func main() {
	opt := &Options{
		endDay:                  0,
		NumDays:                 7,
		TestSuccessThreshold:    99.99,
		MinTestRuns:             10,
		Output:                  "json",
		FailureClusterThreshold: 10,
		StartDay:                0,
		ListenAddr:              ":8080",
	}

	klog.InitFlags(nil)
	flag.CommandLine.Set("skip_headers", "true")

	cmd := &cobra.Command{
		Run: func(cmd *cobra.Command, arguments []string) {
			if err := opt.Complete(); err != nil {
				klog.Exitf("error: %v", err)
			}
			if err := opt.Validate(); err != nil {
				klog.Exitf("error: %v", err)
			}
			if err := opt.Run(); err != nil {
				klog.Exitf("error: %v", err)
			}
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opt.LocalData, "local-data", opt.LocalData, "Path to testgrid data from local disk")
	flags.StringArrayVar(&opt.OpenshiftReleases, "release", opt.OpenshiftReleases, "Which releases to analyze (one per arg instance)")
	flags.StringArrayVar(&opt.Dashboards, "dashboard", opt.Dashboards, "<display-name>=<comma-separated-list-of-dashboards>=<openshift-version>")
	flags.StringArrayVar(&opt.Variants, "variant", opt.Variants, "{ocp,kube,none}")
	flags.IntVar(&opt.StartDay, "start-day", opt.StartDay, "Analyze data starting from this day")
	// TODO convert this to be an offset so that we can go backwards from "data we have"
	flags.IntVar(&opt.endDay, "end-day", opt.endDay, "Look at job runs going back to this day")
	flags.IntVar(&opt.NumDays, "num-days", opt.NumDays, "Look at job runs going back to this many days from the start day")
	flags.Float64Var(&opt.TestSuccessThreshold, "test-success-threshold", opt.TestSuccessThreshold, "Filter results for tests that are more than this percent successful")
	flags.StringVar(&opt.JobFilter, "job-filter", opt.JobFilter, "Only analyze jobs that match this regex")
	flags.StringVar(&opt.FetchData, "fetch-data", opt.FetchData, "Download testgrid data to directory specified for future use with --local-data")
	flags.IntVar(&opt.MinTestRuns, "min-test-runs", opt.MinTestRuns, "Ignore tests with less than this number of runs")
	flags.IntVar(&opt.FailureClusterThreshold, "failure-cluster-threshold", opt.FailureClusterThreshold, "Include separate report on job runs with more than N test failures, -1 to disable")
	flags.StringVarP(&opt.Output, "output", "o", opt.Output, "Output format for report: json, text")
	flag.StringVar(&opt.ListenAddr, "listen", opt.ListenAddr, "The address to serve analysis reports on")
	flags.BoolVar(&opt.Server, "server", opt.Server, "Run in web server mode (serve reports over http)")
	flags.BoolVar(&opt.SkipBugLookup, "skip-bug-lookup", opt.SkipBugLookup, "Do not attempt to find bugs that match test/job failures")

	flags.AddGoFlag(flag.CommandLine.Lookup("v"))
	flags.AddGoFlag(flag.CommandLine.Lookup("skip_headers"))

	if err := cmd.Execute(); err != nil {
		klog.Exitf("error: %v", err)
	}
}

func (o *Options) Complete() error {
	// if the end day was explicitly specified, honor that
	if o.endDay != 0 {
		o.NumDays = o.endDay - o.StartDay
	}
	for _, openshiftRelease := range o.OpenshiftReleases {
		o.Dashboards = append(o.Dashboards, dashboardArgFromOpenshiftRelease(openshiftRelease))
	}

	return nil
}

func (o *Options) ToTestGridDashboardCoordinates() []sippyserver.TestGridDashboardCoordinates {
	dashboards := []sippyserver.TestGridDashboardCoordinates{}
	for _, dashboard := range o.Dashboards {
		tokens := strings.Split(dashboard, "=")
		if len(tokens) != 3 {
			// launch error
			panic(fmt.Sprintf("must have three tokens: %q", dashboard))
		}

		dashboards = append(dashboards,
			sippyserver.TestGridDashboardCoordinates{
				ReportName:             tokens[0],
				TestGridDashboardNames: strings.Split(tokens[1], ","),
				BugzillaRelease:        tokens[2],
			},
		)
	}

	return dashboards
}

// dashboardArgFromOpenshiftRelease converts a --release string into the generic --dashboard arg
func dashboardArgFromOpenshiftRelease(release string) string {
	const openshiftDashboardTemplate = "redhat-openshift-ocp-release-%s-%s"
	dashboards := []string{}
	dashboards = append(dashboards, fmt.Sprintf(openshiftDashboardTemplate, release, "blocking"))
	dashboards = append(dashboards, fmt.Sprintf(openshiftDashboardTemplate, release, "informing"))

	argString := release + "=" + strings.Join(dashboards, ",") + "=" + release
	return argString
}

func (o *Options) Validate() error {
	switch o.Output {
	case "json":
	default:
		return fmt.Errorf("invalid output type: %s\n", o.Output)
	}

	for _, dashboard := range o.Dashboards {
		tokens := strings.Split(dashboard, "=")
		if len(tokens) != 3 {
			return fmt.Errorf("must have three tokens: %q", dashboard)
		}
	}

	if len(o.Variants) > 1 {
		return fmt.Errorf("only one --variant allowed for now")
	} else if len(o.Variants) == 1 {
		if !sets.NewString("ocp", "kube", "none").Has(o.Variants[0]) {
			return fmt.Errorf("only ocp, kube, or none is allowed")
		}
	}

	return nil
}

func (o *Options) Run() error {
	if len(o.FetchData) != 0 {
		dashboards := []string{}
		for _, dashboardCoordinate := range o.ToTestGridDashboardCoordinates() {
			dashboards = append(dashboards, dashboardCoordinate.TestGridDashboardNames...)
		}
		testgridhelpers.DownloadData(dashboards, o.JobFilter, o.FetchData)
		return nil
	}

	if !o.Server {
		return o.runCLIReportMode()
	}

	if o.Server {
		return o.runServerMode()
	}

	return nil
}

func (o *Options) runServerMode() error {
	server := sippyserver.NewServer(
		o.toTestGridLoadingConfig(),
		o.toRawJobResultsAnalysisConfig(),
		o.toDisplayDataConfig(),
		o.ToTestGridDashboardCoordinates(),
		o.ListenAddr,
		o.getSynthenticTestManager(),
		o.getVariantManager(),
		o.getBugCache(),
	)
	server.RefreshData() // force a data refresh once before serving.
	server.Serve()
	return nil
}

func (o *Options) runCLIReportMode() error {
	analyzer := sippyserver.TestReportGeneratorConfig{
		TestGridLoadingConfig:       o.toTestGridLoadingConfig(),
		RawJobResultsAnalysisConfig: o.toRawJobResultsAnalysisConfig(),
		DisplayDataConfig:           o.toDisplayDataConfig(),
	}

	testReport := analyzer.PrepareTestReport(o.ToTestGridDashboardCoordinates()[0], o.getSynthenticTestManager(), o.getVariantManager(), o.getBugCache())

	enc := json.NewEncoder(os.Stdout)
	enc.Encode(testReport.ByTest)
	return nil
}

func (o *Options) hasOCPDashboard() bool {
	for _, dashboardCoordinate := range o.ToTestGridDashboardCoordinates() {
		for _, dashboardName := range dashboardCoordinate.TestGridDashboardNames {
			if strings.Contains(dashboardName, "redhat-openshift-ocp-release-") {
				return true
			}
		}
	}
	return false
}

func (o *Options) getBugCache() buganalysis.BugCache {
	if o.SkipBugLookup || len(o.OpenshiftReleases) == 0 {
		return buganalysis.NewNoOpBugCache()
	} else {
		return buganalysis.NewBugCache()
	}
}

func (o *Options) getVariantManager() testidentification.VariantManager {
	if len(o.Variants) == 0 {
		if o.hasOCPDashboard() {
			return testidentification.NewOpenshiftVariantManager()
		}
		return testidentification.NewEmptyVariantManager()
	}

	// TODO allow more than one with a union
	switch o.Variants[0] {
	case "ocp":
		return testidentification.NewOpenshiftVariantManager()
	case "kube":
		return testidentification.NewKubeVariantManager()
	case "none":
		return testidentification.NewEmptyVariantManager()
	default:
		panic("only ocp, kube, or none is allowed")
	}
}

func (o *Options) getSynthenticTestManager() testgridconversion.SythenticTestManager {
	if o.hasOCPDashboard() {
		return testgridconversion.NewOpenshiftSythenticTestManager()
	}

	return testgridconversion.NewEmptySythenticTestManager()
}

func (o *Options) toTestGridLoadingConfig() sippyserver.TestGridLoadingConfig {
	var jobFilter *regexp.Regexp
	if len(o.JobFilter) > 0 {
		jobFilter = regexp.MustCompile(o.JobFilter)
	}

	return sippyserver.TestGridLoadingConfig{
		LocalData: o.LocalData,
		JobFilter: jobFilter,
	}
}

func (o *Options) toRawJobResultsAnalysisConfig() sippyserver.RawJobResultsAnalysisConfig {
	return sippyserver.RawJobResultsAnalysisConfig{
		StartDay: o.StartDay,
		NumDays:  o.NumDays,
	}
}
func (o *Options) toDisplayDataConfig() sippyserver.DisplayDataConfig {
	return sippyserver.DisplayDataConfig{
		MinTestRuns:             o.MinTestRuns,
		TestSuccessThreshold:    o.TestSuccessThreshold,
		FailureClusterThreshold: o.FailureClusterThreshold,
	}
}
