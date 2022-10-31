package runner

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/kyokomi/emoji"
	"github.com/rs/zerolog/log"

	"github.com/coreruleset/go-ftw/check"
	"github.com/coreruleset/go-ftw/config"
	"github.com/coreruleset/go-ftw/ftwhttp"
	"github.com/coreruleset/go-ftw/test"
	"github.com/coreruleset/go-ftw/utils"
	"github.com/coreruleset/go-ftw/waflog"
)

var errBadTestRequest = errors.New("ftw/run: bad test: choose between data, encoded_request, or raw_request")

// Run runs your tests with the specified Config. Returns error if some test failed
func Run(tests []test.FTWTest, c Config) (TestRunContext, error) {
	printUnlessQuietMode(c.Quiet, ":rocket:Running go-ftw!\n")

	logLines, err := waflog.NewFTWLogLines(waflog.WithLogFile(config.FTWConfig.LogFile))
	if err != nil {
		return TestRunContext{}, err
	}

	conf := ftwhttp.NewClientConfig()
	if c.ConnectTimeout != 0 {
		conf.ConnectTimeout = c.ConnectTimeout
	}
	if c.ReadTimeout != 0 {
		conf.ReadTimeout = c.ReadTimeout
	}
	client, err := ftwhttp.NewClient(conf)
	if err != nil {
		return TestRunContext{}, err
	}
	runContext := TestRunContext{
		Include:  c.Include,
		Exclude:  c.Exclude,
		ShowTime: c.ShowTime,
		Output:   c.Quiet,
		Client:   client,
		LogLines: logLines,
		RunMode:  config.FTWConfig.RunMode,
	}

	for _, tc := range tests {
		if err := RunTest(&runContext, tc); err != nil {
			return TestRunContext{}, err
		}
	}

	printSummary(c.Quiet, runContext.Stats)

	defer cleanLogs(logLines)

	return runContext, nil
}

// RunTest runs an individual test.
// runContext contains information for the current test run
// ftwTest is the test you want to run
func RunTest(runContext *TestRunContext, ftwTest test.FTWTest) error {
	changed := true

	for _, testCase := range ftwTest.Tests {
		// if we received a particular testid, skip until we find it
		if needToSkipTest(runContext.Include, runContext.Exclude, testCase.TestTitle, ftwTest.Meta.Enabled) {
			addResultToStats(Skipped, testCase.TestTitle, &runContext.Stats)
			if !ftwTest.Meta.Enabled {
				printUnlessQuietMode(runContext.Output, "\tskipping %s\n", testCase.TestTitle)
			}
			continue
		}
		// this is just for printing once the next test
		if changed {
			printUnlessQuietMode(runContext.Output, ":point_right:executing tests in file %s\n", ftwTest.Meta.Name)
			changed = false
		}

		// can we use goroutines here?
		printUnlessQuietMode(runContext.Output, "\trunning %s: ", testCase.TestTitle)
		// Iterate over stages
		for _, stage := range testCase.Stages {
			ftwCheck := check.NewCheck(config.FTWConfig)
			if err := RunStage(runContext, ftwCheck, testCase, stage.Stage); err != nil {
				return err
			}
		}
	}

	return nil
}

// RunStage runs an individual test stage.
// runContext contains information for the current test run
// ftwCheck is the current check utility
// testCase is the test case the stage belongs to
// stage is the stage you want to run
func RunStage(runContext *TestRunContext, ftwCheck *check.FTWCheck, testCase test.Test, stage test.Stage) error {
	stageStartTime := time.Now()
	stageID := uuid.NewString()
	// Apply global overrides initially
	testRequest := stage.Input
	err := applyInputOverride(&testRequest)
	if err != nil {
		log.Debug().Msgf("ftw/run: problem overriding input: %s", err.Error())
	}
	expectedOutput := stage.Output

	// Check sanity first
	if checkTestSanity(testRequest) {
		return errBadTestRequest
	}

	// Do not even run test if result is overridden. Just use the override and display the overridden result.
	if overridden := overriddenTestResult(ftwCheck, testCase.TestTitle); overridden != Failed {
		addResultToStats(overridden, testCase.TestTitle, &runContext.Stats)
		displayResult(runContext.Output, overridden, time.Duration(0), time.Duration(0))
		return nil
	}

	var req *ftwhttp.Request

	// Destination is needed for an request
	dest := &ftwhttp.Destination{
		DestAddr: testRequest.GetDestAddr(),
		Port:     testRequest.GetPort(),
		Protocol: testRequest.GetProtocol(),
	}

	if notRunningInCloudMode(ftwCheck) {
		startMarker, err := markAndFlush(runContext, dest, stageID)
		if err != nil && !expectedOutput.ExpectError {
			return fmt.Errorf("failed to find start marker: %w", err)
		}
		ftwCheck.SetStartMarker(startMarker)
	}

	req = getRequestFromTest(testRequest)

	err = runContext.Client.NewConnection(*dest)

	if err != nil && !expectedOutput.ExpectError {
		return fmt.Errorf("can't connect to destination %+v: %w", dest, err)
	}
	runContext.Client.StartTrackingTime()

	response, responseErr := runContext.Client.Do(*req)

	runContext.Client.StopTrackingTime()
	if responseErr != nil && !expectedOutput.ExpectError {
		return fmt.Errorf("failed sending request to destination %+v: %w", dest, responseErr)
	}

	if notRunningInCloudMode(ftwCheck) {
		endMarker, err := markAndFlush(runContext, dest, stageID)
		if err != nil && !expectedOutput.ExpectError {
			return fmt.Errorf("failed to find end marker: %w", err)

		}
		ftwCheck.SetEndMarker(endMarker)
	}

	// Set expected test output in check
	ftwCheck.SetExpectTestOutput(&expectedOutput)

	// now get the test result based on output
	testResult := checkResult(ftwCheck, response, responseErr)

	roundTripTime := runContext.Client.GetRoundTripTime().RoundTripDuration()
	stageTime := time.Since(stageStartTime)

	addResultToStats(testResult, testCase.TestTitle, &runContext.Stats)

	runContext.Result = testResult

	// show the result unless quiet was passed in the command line
	displayResult(runContext.Output, testResult, roundTripTime, stageTime)

	runContext.Stats.Run++
	runContext.Stats.RunTime += stageTime

	return nil
}

func markAndFlush(runContext *TestRunContext, dest *ftwhttp.Destination, stageID string) ([]byte, error) {
	rline := &ftwhttp.RequestLine{
		Method: "GET",
		// Use the `/status` endpoint of `httpbin` (http://httpbin.org), if possible,
		// to minimize the amount of data transferred and in the log.
		// `httpbin` is used by the CRS test setup.
		URI:     "/status/200",
		Version: "HTTP/1.1",
	}

	headers := &ftwhttp.Header{
		"Accept":                             "*/*",
		"User-Agent":                         "go-ftw test agent",
		"Host":                               "localhost",
		config.FTWConfig.LogMarkerHeaderName: stageID,
	}

	req := ftwhttp.NewRequest(rline, *headers, nil, true)

	// 20 is a very conservative number. The web server should flush its
	// buffer a lot earlier but we have absolutely no control over that.
	for range [20]int{} {
		err := runContext.Client.NewOrReusedConnection(*dest)
		if err != nil {
			return nil, fmt.Errorf("ftw/run: can't connect to destination %+v: %w", dest, err)
		}

		_, err = runContext.Client.Do(*req)
		if err != nil {
			return nil, fmt.Errorf("ftw/run: failed sending request to %+v: %w", dest, err)
		}

		marker := runContext.LogLines.CheckLogForMarker(stageID)
		if marker != nil {
			return marker, nil
		}
	}
	return nil, fmt.Errorf("can't find log marker. Am I reading the correct log? Log file: %s", runContext.LogLines.FileName)
}

func needToSkipTest(include *regexp.Regexp, exclude *regexp.Regexp, title string, enabled bool) bool {
	// skip disabled tests
	if !enabled {
		return true
	}

	// never skip enabled explicit inclusions
	if include != nil {
		if include.MatchString(title) {
			// inclusion always wins over exclusion
			return false
		}
	}

	result := false
	// if we need to exclude tests, and the title matches,
	// it needs to be skipped
	if exclude != nil {
		if exclude.MatchString(title) {
			result = true
		}
	}

	// if we need to include tests, but the title does not match
	// it needs to be skipped
	if include != nil {
		if !include.MatchString(title) {
			result = true
		}
	}

	return result
}

func checkTestSanity(testRequest test.Input) bool {
	return (utils.IsNotEmpty(testRequest.Data) && testRequest.EncodedRequest != "") ||
		(utils.IsNotEmpty(testRequest.Data) && testRequest.RAWRequest != "") ||
		(testRequest.EncodedRequest != "" && testRequest.RAWRequest != "")
}

func displayResult(quiet bool, result TestResult, roundTripTime time.Duration, stageTime time.Duration) {
	switch result {
	case Success:
		printUnlessQuietMode(quiet, ":check_mark:passed in %s (RTT %s)\n", stageTime, roundTripTime)
	case Failed:
		printUnlessQuietMode(quiet, ":collision:failed in %s (RTT %s)\n", stageTime, roundTripTime)
	case Ignored:
		printUnlessQuietMode(quiet, ":information:test ignored\n")
	case ForceFail:
		printUnlessQuietMode(quiet, ":information:test forced to fail\n")
	case ForcePass:
		printUnlessQuietMode(quiet, ":information:test forced to pass\n")
	default:
		// don't print anything if skipped test
	}
}

func overriddenTestResult(c *check.FTWCheck, id string) TestResult {
	if c.ForcedIgnore(id) {
		return Ignored
	}

	if c.ForcedFail(id) {
		return ForceFail
	}

	if c.ForcedPass(id) {
		return ForcePass
	}

	return Failed
}

// checkResult has the logic for verifying the result for the test sent
func checkResult(c *check.FTWCheck, response *ftwhttp.Response, responseError error) TestResult {
	// Request might return an error, but it could be expected, we check that first
	if responseError != nil && c.AssertExpectError(responseError) {
		return Success
	}

	// If there was no error, perform the remaining checks
	if responseError != nil {
		return Failed
	}
	if c.CloudMode() {
		// Cloud mode assumes that we cannot read logs. So we rely entirely on status code
		c.SetCloudMode()
	}

	// If we didn't expect an error, check the actual response from the waf
	if response != nil {
		if c.AssertStatus(response.Parsed.StatusCode) {
			return Success
		}
		// Check response
		if c.AssertResponseContains(response.GetBodyAsString()) {
			return Success
		}
	}
	// Lastly, check logs
	if c.AssertLogContains() {
		return Success
	}
	// We assume that the they were already setup, for comparing
	if c.AssertNoLogContains() {
		return Success
	}

	return Failed
}

func getRequestFromTest(testRequest test.Input) *ftwhttp.Request {
	var req *ftwhttp.Request
	// get raw request, if anything
	raw, err := testRequest.GetRawRequest()
	if err != nil {
		log.Error().Msgf("ftw/run: error getting raw data: %s\n", err.Error())
	}

	// If we use raw or encoded request, then we don't use other fields
	if raw != nil {
		req = ftwhttp.NewRawRequest(raw, !testRequest.StopMagic)
	} else {
		rline := &ftwhttp.RequestLine{
			Method:  testRequest.GetMethod(),
			URI:     testRequest.GetURI(),
			Version: testRequest.GetVersion(),
		}

		data := testRequest.ParseData()
		// create a new request
		req = ftwhttp.NewRequest(rline, testRequest.Headers,
			data, !testRequest.StopMagic)

	}
	return req
}

// We want to have output unless we are in quiet mode
func printUnlessQuietMode(quiet bool, format string, a ...interface{}) {
	if !quiet {
		emoji.Printf(format, a...)
	}
}

// applyInputOverride will check if config had global overrides and write that into the test.
func applyInputOverride(testRequest *test.Input) error {
	overrides := config.FTWConfig.TestOverride.Input
	if overrides.Port != nil {
		testRequest.Port = overrides.Port
	}
	if overrides.DestAddr != nil {
		testRequest.DestAddr = overrides.DestAddr
		if testRequest.Headers == nil {
			testRequest.Headers = ftwhttp.Header{}
		}
		if testRequest.Headers.Get("Host") == "" {
			testRequest.Headers.Set("Host", *overrides.DestAddr)
		}
	}
	if overrides.Protocol != nil {
		testRequest.Protocol = overrides.Protocol
	}

	return nil
}

func notRunningInCloudMode(c *check.FTWCheck) bool {
	return !c.CloudMode()
}

func cleanLogs(logLines *waflog.FTWLogLines) {
	if err := logLines.Cleanup(); err != nil {
		log.Error().Err(err).Msg("Failed to cleanup log file")
	}
}
