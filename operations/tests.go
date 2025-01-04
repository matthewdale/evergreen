package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/mongodb/grip"
	"github.com/urfave/cli"
)

/*
User stories:
- I'm a DBX dev and I'm troubleshooting a flaky test in a task. I want to know
  how often it's failed recently.
	- E.g. "TestClientStress/Client_recovers_from_traffic_spike/maxPoolSize_100" - fails a lot, Windows-only
	- E.g. "TestCollection/insert_many/large_document_batches" - fails intermittently, only in race detector
	- I want to be able to search by prefix, or maybe any pattern.
	- For less common failures, "When is the last time the test failed?"
	- For more common failures, "For the last N patches (versions), how many times did it fail?"
	- The above could be separate commands or the same command. E.g. "evergreen tests lastfail [name]"

- I'm a DBX dev and I'm troubleshooting a task that fails with a particular
  logged message. I want to know how often it's failed recently

- I'm a DBX dev and I'm working on greener build day. I want to know the most
  frequently failing tasks on the waterfall so I can fix them.
	- Probably can just be an ordered list of failure frequencies and test names.
	- E.g. "evergreen tests topfail"
*/

func Tests() cli.Command {
	const (
		versionsFlagName    = "versions"
		limitFlagName       = "limit"
		testFlagName        = "test"
		showSummaryFlagName = "show-summary"
	)

	return cli.Command{
		Name:    "tests",
		Aliases: []string{"t"},
		Usage:   "waterfall build test statistics",
		Flags: mergeFlagSlices(
			addProjectFlag(),
		),
		Subcommands: []cli.Command{
			{
				Name:  "topfail",
				Usage: "find the most frequent test failures in recent waterfall builds",
				Flags: mergeFlagSlices(
					addProjectFlag(),
					[]cli.Flag{
						cli.IntFlag{
							Name:  versionsFlagName,
							Usage: "number of patches to show (0 for all patches)",
							Value: 6,
						},
						cli.IntFlag{
							Name:  limitFlagName,
							Usage: "number of most frequent test failures to show",
							Value: 20,
						},
					}),
				Action: func(c *cli.Context) error {
					confPath := c.Parent().String(confFlagName)
					projectID := c.String(projectFlagName)
					versions := c.Int(versionsFlagName)
					limit := c.Int(limitFlagName)

					conf, err := NewClientSettings(confPath)
					if err != nil {
						return fmt.Errorf("error loading configuration: %w", err)
					}

					if projectID == "" {
						grip.Debug("No project ID specified, trying to find default project for cwd")

						cwd, err := os.Getwd()
						if err != nil {
							return fmt.Errorf("error getting cwd: %w", err)
						}
						cwd, err = filepath.EvalSymlinks(cwd)
						if err != nil {
							return fmt.Errorf("error evaluating symlinks for cwd: %w", err)
						}

						grip.Debugf("Trying to find default project for dir %q", cwd)

						projectID = conf.FindDefaultProject(cwd, false)
					}
					if projectID == "" {
						return errors.New("need to specify a project")
					}

					infos, err := getInfos(
						context.Background(),
						conf.User,
						conf.APIKey,
						projectID,
						versions)
					if err != nil {
						return fmt.Errorf("error getting revision info: %w", err)
					}

					type failedTestInfo struct {
						Test                   string
						FailedTasksPerRevision map[string][]string
						FailuresPerRevision    map[string]int
						TotalFailures          int
					}

					tests := make(map[string]*failedTestInfo) // map[test]failureStats

					for _, info := range infos {
						for _, variant := range info.FailedVariants {
							for _, task := range variant.FailedTasks {
								for _, test := range filterTests(task.FailedTests) {
									if tests[test] == nil {
										tests[test] = &failedTestInfo{
											FailuresPerRevision:    make(map[string]int),
											FailedTasksPerRevision: make(map[string][]string),
										}
									}
									tests[test].Test = test

									tasks := tests[test].FailedTasksPerRevision[info.Revision]
									tasks = append(tasks, task.Task)
									tests[test].FailedTasksPerRevision[info.Revision] = tasks

									tests[test].FailuresPerRevision[info.Revision]++
									tests[test].TotalFailures++
								}
							}
						}
					}

					testInfos := slices.Collect(maps.Values(tests))
					sort.Slice(testInfos, func(i, j int) bool { return testInfos[i].TotalFailures > testInfos[j].TotalFailures })

					if limit >= 0 && len(testInfos) > limit {
						n := limit
						if n >= len(testInfos) {
							n = len(testInfos) - 1
						}
						testInfos = testInfos[:n]
					}

					fmt.Println()

					w := new(tabwriter.Writer)
					// Format in tab-separated columns with a tab stop of 8.
					w.Init(os.Stdout, 0, 8, 0, '\t', 0)
					fmt.Fprintln(w, "\tCount\tTest Name")
					for _, info := range testInfos {
						line := fmt.Sprintf("\t%v\t%v", info.TotalFailures, info.Test)
						fmt.Fprintln(w, line)
					}

					return w.Flush()
				},
			},
			{
				Name:  "failstats",
				Usage: "show how many times a specific test fails per version, variant, and task",
				Flags: mergeFlagSlices(
					addProjectFlag(),
					[]cli.Flag{
						cli.IntFlag{
							Name:  joinFlagNames(versionsFlagName, "l"),
							Usage: "number of patches to show (0 for all patches)",
							Value: 6,
						},
						cli.StringFlag{
							Name:     joinFlagNames(testFlagName, "n"),
							Usage:    "the test name to filter for",
							Required: true,
						},
					}),
				Action: func(c *cli.Context) error {
					confPath := c.Parent().String(confFlagName)
					limit := c.Int(versionsFlagName)
					projectID := c.String(projectFlagName)
					testName := c.String(testFlagName)

					conf, err := NewClientSettings(confPath)
					if err != nil {
						return fmt.Errorf("error loading configuration: %w", err)
					}

					if projectID == "" {
						grip.Debug("No project ID specified, trying to find default project for cwd")

						cwd, err := os.Getwd()
						if err != nil {
							return fmt.Errorf("error getting cwd: %w", err)
						}
						cwd, err = filepath.EvalSymlinks(cwd)
						if err != nil {
							return fmt.Errorf("error evaluating symlinks for cwd: %w", err)
						}

						grip.Debugf("Trying to find default project for dir %q", cwd)

						projectID = conf.FindDefaultProject(cwd, false)
					}
					if projectID == "" {
						return errors.New("need to specify a project")
					}

					infos, err := getInfos(
						context.Background(),
						conf.User,
						conf.APIKey,
						projectID,
						limit,
					)
					if err != nil {
						return fmt.Errorf("error getting revision info: %w", err)
					}

					versions := make(map[string]int)
					variants := make(map[string]int)
					tasks := make(map[string]int)
					for _, info := range infos {
						// versionInfo := fmt.Sprintf("https://spruce.mongodb.com/version/%s Created:%v", info.VersionID, info.Created)
						for _, variant := range info.FailedVariants {
							// variantInfo := fmt.Sprintf("Variant:%v", variant.DisplayName)
							for _, task := range variant.FailedTasks {
								// taskInfo := fmt.Sprintf("Task:%v", task.Task)
								for _, test := range task.FailedTests {
									if !strings.Contains(test, testName) {
										continue
									}
									// if versionInfo != "" {
									// 	fmt.Println(versionInfo)
									// 	versionInfo = ""
									// }
									// if variantInfo != "" {
									// 	fmt.Println(variantInfo)
									// 	variantInfo = ""
									// }
									// if taskInfo != "" {
									// 	fmt.Println(taskInfo)
									// 	taskInfo = ""
									// }
									versions[info.VersionID]++
									variants[variant.DisplayName]++
									tasks[task.Task]++
								}
							}
						}
					}

					printColumns := func(header string, rows map[string]int) {
						w := new(tabwriter.Writer)
						// Format in tab-separated columns with a tab stop of 8.
						w.Init(os.Stdout, 0, 8, 0, '\t', 0)
						fmt.Fprintln(w, header)

						type tuple struct {
							k string
							v int
						}

						tup := make([]tuple, 0, len(rows))

						for k, v := range rows {
							tup = append(tup, tuple{k: k, v: v})
						}
						sort.Slice(tup, func(i, j int) bool { return tup[i].v > tup[j].v })

						for _, t := range tup {
							line := fmt.Sprintf("\t%v\t%v", t.v, t.k)
							fmt.Fprintln(w, line)
						}
						w.Flush()
					}

					printColumns("\tCount\tVersion", versions)
					fmt.Println()
					printColumns("\tCount\tVariant", variants)
					fmt.Println()
					printColumns("\tCount\tTask", tasks)

					return nil
				},
			},
		},
	}
}

const (
	mainlineFailuresQuery = `
  query MainlineCommits(
	$mainlineCommitsOptions: MainlineCommitsOptions!
	$buildVariantOptions: BuildVariantOptions!
	$buildVariantOptionsForGraph: BuildVariantOptions!
	$buildVariantOptionsForTaskIcons: BuildVariantOptions!
	$buildVariantOptionsForGroupedTasks: BuildVariantOptions!
  ) {
	mainlineCommits(
	  options: $mainlineCommitsOptions
	  buildVariantOptions: $buildVariantOptions
	) {
	  nextPageOrderNumber
	  prevPageOrderNumber
	  versions {
		rolledUpVersions {
		  author
		  createTime
		  id
		  ignored
		  message
		  order
		  revision
		  __typename
		}
		version {
		  author
		  buildVariants(options: $buildVariantOptionsForTaskIcons) {
			displayName
			tasks {
			  displayName
			  execution
			  hasCedarResults
			  id
			  status
			  timeTaken
			  __typename
			}
			variant
			__typename
		  }
		  buildVariantStats(options: $buildVariantOptionsForGroupedTasks) {
			displayName
			statusCounts {
			  count
			  status
			  __typename
			}
			variant
			__typename
		  }
		  createTime
		  gitTags {
			pusher
			tag
			__typename
		  }
		  id
		  message
		  order
		  projectIdentifier
		  revision
		  taskStatusStats(options: $buildVariantOptionsForGraph) {
			counts {
			  count
			  status
			  __typename
			}
			eta
			__typename
		  }
		  ...UpstreamProject
		  __typename
		}
		__typename
	  }
	  __typename
	}
  }
  
  fragment UpstreamProject on Version {
	upstreamProject {
	  owner
	  project
	  repo
	  revision
	  task {
		execution
		id
		__typename
	  }
	  triggerID
	  triggerType
	  version {
		id
		__typename
	  }
	  __typename
	}
	__typename
  }`

	taskTestSampleQuery = `
  query ($versionId: String!, $taskIds: [String!]!, $filters: [TestFilter!]!) {
	taskTestSample(versionId: $versionId, taskIds: $taskIds, filters: $filters) {
	  execution
	  matchingFailedTestNames
	  taskId
	  totalTestCount
	}
  }`
)

type revisionInfo struct {
	VersionID      string
	Created        time.Time
	Revision       string
	Message        string
	FailedVariants []variantInfo
}

type variantInfo struct {
	DisplayName string
	FailedTasks []taskInfo
}

type taskInfo struct {
	Task        string
	FailedTests []string
}

func getInfos(
	ctx context.Context,
	user, apiKey, projectID string,
	versions int,
) ([]revisionInfo, error) {
	// Define the types required to unmarshal the mainlineCommits GraphQL
	// response.
	type taskRes struct {
		DisplayName string `json:"displayName"`
		Execution   int    `json:"execution"`
		ID          string `json:"id"`
		Status      string `json:"status"`
	}
	type buildVariant struct {
		DisplayName string    `json:"displayName"`
		Tasks       []taskRes `json:"tasks"`
	}
	type versionRes struct {
		ID            string         `json:"id"`
		Revision      string         `json:"revision"`
		Message       string         `json:"message"`
		BuildVariants []buildVariant `json:"buildVariants"`
		CreateTime    time.Time      `json:"createTime"`
	}
	type mainlineCommitVersion struct {
		Version versionRes `json:"version"`
	}
	type mainlineCommits struct {
		Versions []mainlineCommitVersion `json:"versions"`
	}

	// Run the mainlineCommits query that returns a summary of the failures in
	// the last N mainline versions (i.e. waterfall builds) for the given
	// project ID.
	mainlineFailuresVars := map[string]any{
		"mainlineCommitsOptions": map[string]any{
			"projectIdentifier": projectID,
			"limit":             versions,
			"shouldCollapse":    false,
			"requesters":        []string{},
		},
		"buildVariantOptions": map[string]any{
			"tasks":            []string{},
			"variants":         []string{},
			"statuses":         []string{},
			"includeBaseTasks": false,
		},
		"buildVariantOptionsForGraph": map[string]any{
			"statuses": []string{},
			"tasks":    []string{},
			"variants": []string{},
		},
		"buildVariantOptionsForGroupedTasks": map[string]any{
			"tasks":    []string{"^\b$"},
			"variants": []string{},
			"statuses": []string{},
		},
		"buildVariantOptionsForTaskIcons": map[string]any{
			"tasks":    []string{},
			"variants": []string{},
			"statuses": []string{
				"failed",
				"task-timed-out",
				"test-timed-out",
				"known-issue",
				"setup-failed",
				"system-failed",
				"system-timed-out",
				"system-unresponsive",
				"aborted",
			},
			"includeBaseTasks": false,
		},
	}
	resJSON, err := graphql(
		ctx,
		user,
		apiKey,
		mainlineFailuresQuery,
		mainlineFailuresVars)
	if err != nil {
		return nil, fmt.Errorf("error querying mainlineCommits: %w", err)
	}
	var res struct {
		Data struct {
			MainlineCommits mainlineCommits `json:"mainlineCommits"`
		} `json:"data"`
	}
	err = json.Unmarshal(resJSON, &res)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling mainlineCommits: %w", err)
	}

	// Define the type required to unmarshal the taskTestSample GraphQL
	// responses.
	type taskTestSample struct {
		Execution               int      `json:"execution"`
		MatchingFailedTestNames []string `json:"matchingFailedTestNames"`
		TaskID                  string   `json:"taskId"`
		TotalTestCount          int      `json:"totalTestCount"`
	}

	// For each version, run the taskTestSample query to get the failed test
	// names for all tasks in that version.
	infos := make([]revisionInfo, 0)
	for _, ver := range res.Data.MainlineCommits.Versions {
		failedVariants := make([]variantInfo, 0)
		for _, variant := range ver.Version.BuildVariants {
			taskIDs := make(map[string]string, len(variant.Tasks)) // map[taskId]displayName
			for _, t := range variant.Tasks {
				taskIDs[t.ID] = t.DisplayName
			}

			resJSON, err := graphql(
				ctx,
				user,
				apiKey,
				taskTestSampleQuery,
				map[string]any{
					"versionId": ver.Version.ID,
					"taskIds":   slices.Collect(maps.Keys(taskIDs)),
					"filters":   []string{},
				})
			if err != nil {
				return nil, fmt.Errorf("error querying taskTestSample: %w", err)
			}

			var res struct {
				Data struct {
					TaskTestSample []taskTestSample `json:"taskTestSample"`
				} `json:"data"`
			}
			err = json.Unmarshal(resJSON, &res)
			if err != nil {
				return nil, fmt.Errorf("error unmarshaling taskTestSample: %w", err)
			}

			grip.Debugln("Version ID:", ver.Version.ID, "Task IDs:", taskIDs, "Failing Tests:")

			failedTasks := make([]taskInfo, 0)
			for _, sample := range res.Data.TaskTestSample {
				grip.Debugf("Version:", ver.Version.ID, "Task:", taskIDs[sample.TaskID])
				for _, test := range sample.MatchingFailedTestNames {
					grip.Debugln(test)
				}
				failedTasks = append(failedTasks, taskInfo{
					Task:        taskIDs[sample.TaskID],
					FailedTests: sample.MatchingFailedTestNames,
				})
			}
			failedVariants = append(failedVariants, variantInfo{
				DisplayName: variant.DisplayName,
				FailedTasks: failedTasks,
			})
		}

		infos = append(infos, revisionInfo{
			VersionID:      ver.Version.ID,
			Created:        ver.Version.CreateTime,
			Revision:       ver.Version.Revision,
			Message:        ver.Version.Message,
			FailedVariants: failedVariants,
		})
	}

	return infos, nil
}

// TODO: Re-add caching.
// func cached() {
// 	// Caching.
// 	// TODO: Remove?
// 	{
// 		const cacheFilePrefix = ".evergreen_cache_tests_"
// 		h := sha1.New()
// 		h.Write(body)
// 		sfx := hex.EncodeToString(h.Sum(nil))
// 		if b, err := os.ReadFile(cacheFilePrefix + sfx); err == nil {
// 			return b, nil
// 		}
// 		defer func() {
// 			if err != nil {
// 				return
// 			}
// 			os.WriteFile(cacheFilePrefix+sfx, data, 0666)
// 		}()
// 	}
// }

// graphql queries the Evergreen GraphQL API using the provided user creds,
// query, and variables. It returns the response body as a byte slice.
func graphql(
	ctx context.Context,
	user string,
	apiKey string,
	query string,
	variables map[string]any,
) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return nil, fmt.Errorf("error marshaling variables: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://evergreen.mongodb.com/graphql/query",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("error building GraphQL query: %w", err)
	}
	req.Header.Add(evergreen.APIUserHeader, user)
	req.Header.Add(evergreen.APIKeyHeader, apiKey)
	req.Header.Add("content-type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error querying GraphQL API: %w", err)
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

// func (fti *failedTestInfo) String() string {
// 	if fti == nil {
// 		return fmt.Sprint(nil)
// 	}

// 	return fmt.Sprintf("%s: %+v", fti.Test, struct {
// 		// FailedTasksPerRevision map[string][]string
// 		FailuresPerRevision map[string]int
// 		TotalFailures       int
// 	}{
// 		// FailedTasksPerRevision: fti.FailedTasksPerRevision,
// 		FailuresPerRevision: fti.FailuresPerRevision,
// 		TotalFailures:       fti.TotalFailures,
// 	})
// }

func filterTests(tests []string) []string {
	sort.Strings(tests)

	res := make([]string, 0, len(tests))
	for i := range tests {
		if i >= len(tests)-1 || strings.HasPrefix(tests[i+1], tests[i]+"/") {
			continue
		}
		res = append(res, tests[i])
	}
	return res
}
