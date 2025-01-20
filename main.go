package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v2"
)

// Providers we want to check against specific minimum versions
var providersToCheck = map[string]string{
	"azurerm": "4.0.0",
	"azapi":   "2.0.0",
}

// CSVRecord represents a row from the input CSV.
type CSVRecord struct {
	ProviderNamespace               string
	ResourceType                    string
	ModuleDisplayName               string
	AlternativeNames                string
	ModuleName                      string
	ModuleStatus                    string
	RepoURL                         string
	PublicRegistryReference         string
	TelemetryIdPrefix               string
	PrimaryModuleOwnerGHHandle      string
	PrimaryModuleOwnerDisplayName   string
	SecondaryModuleOwnerGHHandle    string
	SecondaryModuleOwnerDisplayName string
	ModuleOwnersGHTeam              string
	ModuleContributorsGHTeam        string
	Description                     string
	Comments                        string
	FirstPublishedIn                string
}

// ProviderVersion represents a single provider requirement (like azurerm, ~> 3.0).
type ProviderVersion struct {
	ProviderName string `json:"provider_name"`
	Version      string `json:"version"`
}

// Result holds the analysis result for each module.
type Result struct {
	CSVRecord
	Providers        []ProviderVersion `json:"providers"`
	Compatibility    map[string]bool   `json:"compatibility"`
	LastCommitDate   string            `json:"last_commit_date,omitempty"`
	LastCommitAuthor string            `json:"last_commit_author,omitempty"`
	Error            string            `json:"error,omitempty"` // Store any processing errors
}

// readCSV reads the input CSV file and returns a slice of CSVRecord.
func readCSV(filename string) ([]CSVRecord, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.TrimLeadingSpace = true

	// Read header
	headers, err := reader.Read()
	if err == io.EOF {
		return nil, fmt.Errorf("empty CSV file")
	}
	if err != nil {
		return nil, err
	}

	// Map header to indices
	headerMap := make(map[string]int)
	for idx, header := range headers {
		headerMap[header] = idx
	}

	var records []CSVRecord
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		record := CSVRecord{
			ProviderNamespace:               getField(row, headerMap, "ProviderNamespace"),
			ResourceType:                    getField(row, headerMap, "ResourceType"),
			ModuleDisplayName:               getField(row, headerMap, "ModuleDisplayName"),
			AlternativeNames:                getField(row, headerMap, "AlternativeNames"),
			ModuleName:                      getField(row, headerMap, "ModuleName"),
			ModuleStatus:                    getField(row, headerMap, "ModuleStatus"),
			RepoURL:                         getField(row, headerMap, "RepoURL"),
			PublicRegistryReference:         getField(row, headerMap, "PublicRegistryReference"),
			TelemetryIdPrefix:               getField(row, headerMap, "TelemetryIdPrefix"),
			PrimaryModuleOwnerGHHandle:      getField(row, headerMap, "PrimaryModuleOwnerGHHandle"),
			PrimaryModuleOwnerDisplayName:   getField(row, headerMap, "PrimaryModuleOwnerDisplayName"),
			SecondaryModuleOwnerGHHandle:    getField(row, headerMap, "SecondaryModuleOwnerGHHandle"),
			SecondaryModuleOwnerDisplayName: getField(row, headerMap, "SecondaryModuleOwnerDisplayName"),
			ModuleOwnersGHTeam:              getField(row, headerMap, "ModuleOwnersGHTeam"),
			ModuleContributorsGHTeam:        getField(row, headerMap, "ModuleContributorsGHTeam"),
			Description:                     getField(row, headerMap, "Description"),
			Comments:                        getField(row, headerMap, "Comments"),
			FirstPublishedIn:                getField(row, headerMap, "FirstPublishedIn"),
		}
		records = append(records, record)
	}
	return records, nil
}

// getField safely retrieves a field from a row based on header map.
func getField(row []string, headerMap map[string]int, field string) string {
	if idx, ok := headerMap[field]; ok && idx < len(row) {
		return row[idx]
	}
	return ""
}

// cloneRepo performs a shallow clone of the repository to a temporary directory.
func cloneRepo(ctx context.Context, repoURL string) (string, error) {
	tempDir, err := os.MkdirTemp("", "repo-*")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", repoURL, tempDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("git clone failed: %v, output: %s", err, string(output))
	}
	return tempDir, nil
}

// parseTerraformModule loads the Terraform module and extracts required providers.
func parseTerraformModule(modulePath string) ([]ProviderVersion, error) {
	module, diags := tfconfig.LoadModule(modulePath)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to load module: %v", diags)
	}
	var result []ProviderVersion
	for providerName, req := range module.RequiredProviders {
		for _, constraint := range req.VersionConstraints {
			result = append(result, ProviderVersion{
				ProviderName: providerName,
				Version:      constraint,
			})
		}
	}
	return result, nil
}

// checkVersionConstraints uses go-version to determine if the constraints are satisfied.
func checkVersionConstraints(currentProviderVersion, constraint string) (bool, error) {
	ver, err := version.NewVersion(currentProviderVersion)
	if err != nil {
		return false, fmt.Errorf("failed to parse version '%s': %v", currentProviderVersion, err)
	}
	c, err := version.NewConstraint(constraint)
	if err != nil {
		return false, fmt.Errorf("failed to parse constraint '%s': %v", constraint, err)
	}
	return c.Check(ver), nil
}

// getLastCommitInfo extracts the last commit epoch time and author from the local repo.
func getLastCommitInfo(ctx context.Context, repoPath string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "log", "-1", "--format=%ct|%an")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("failed to get last commit: %v", err)
	}
	parts := strings.SplitN(string(out), "|", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected format for last commit info")
	}
	epochStr := strings.TrimSpace(parts[0])
	author := strings.TrimSpace(parts[1])
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		return "", "", fmt.Errorf("invalid epoch time '%s': %v", epochStr, err)
	}
	t := time.Unix(epoch, 0)
	return t.Format(time.RFC3339), author, nil
}

// processRecord handles cloning, parsing, and checking for a single CSVRecord.
func processRecord(ctx context.Context, record CSVRecord, quiet bool) Result {
	res := Result{CSVRecord: record}

	// Clone the repository with up to 3 retries
	var repoPath string
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		repoPath, err = cloneRepo(ctx, record.RepoURL)
		if err == nil {
			break
		}
		// If not quiet, log a warning
		if !quiet {
			log.Printf("[WARN] Attempt %d: Failed to clone repo '%s': %v", attempt, record.RepoURL, err)
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		res.Error = fmt.Sprintf("failed to clone repo '%s': %v", record.RepoURL, err)
		return res
	}
	defer os.RemoveAll(repoPath)

	// Parse the Terraform module
	providers, err := parseTerraformModule(repoPath)
	if err != nil {
		res.Error = fmt.Sprintf("failed to parse Terraform module: %v", err)
		return res
	}
	res.Providers = providers

	// Check version constraints
	res.Compatibility = make(map[string]bool)
	for _, provider := range providers {
		if constraint, ok := providersToCheck[provider.ProviderName]; ok {
			valid, cErr := checkVersionConstraints(constraint, provider.Version)
			if cErr != nil {
				res.Error = fmt.Sprintf("failed to check version constraints for provider '%s': %v", provider.ProviderName, cErr)
				return res
			}
			res.Compatibility[provider.ProviderName] = valid
		}
	}

	// Get last commit info
	lastDate, author, commitErr := getLastCommitInfo(ctx, repoPath)
	if commitErr != nil {
		if !quiet {
			log.Printf("[WARN] Could not retrieve last commit for '%s': %v", record.RepoURL, commitErr)
		}
		res.Error = fmt.Sprintf("%s | could not retrieve last commit info: %v", res.Error, commitErr)
	} else {
		res.LastCommitDate = lastDate
		res.LastCommitAuthor = author
	}

	return res
}

// writeJSON writes the results to an output JSON file (unescaped).
func writeJSON(filename string, results []Result) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	encoder.SetEscapeHTML(false) // keep characters like '>' or '~>' as is
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

// downloadCSV fetches the CSV file from the given URL and saves it to a local file.
func downloadCSV(url, filename string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download CSV: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download CSV: received status code %d", resp.StatusCode)
	}
	out, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to save CSV: %v", err)
	}
	return nil
}

// downloadCSVIfNeeded is a helper to conditionally download the CSV.
func downloadCSVIfNeeded(url, filename string, force bool) error {
	if _, err := os.Stat(filename); os.IsNotExist(err) || force {
		if err := downloadCSV(url, filename); err != nil {
			return fmt.Errorf("error downloading CSV: %v", err)
		}
		fmt.Println("CSV source downloaded successfully.")
	} else if err != nil {
		return fmt.Errorf("error checking file: %v", err)
	} else {
		fmt.Printf("CSV source file '%s' already exists, skipping download.\n", filename)
	}
	return nil
}

// summarizeResults returns the counts for unreachable, not-compatible, and dormant repos.
func summarizeResults(results []Result) (int, int, int) {
	var unreachableCount, notCompatibleCount, dormantCount int

	now := time.Now()
	sixMonthsAgo := now.AddDate(0, -6, 0)

	for _, r := range results {
		// Unreachable if we failed to clone
		if strings.Contains(r.Error, "failed to clone repo") {
			unreachableCount++
		}
		// Dormant if last commit was older than 6 months
		if r.LastCommitDate != "" {
			t, parseErr := time.Parse(time.RFC3339, r.LastCommitDate)
			if parseErr == nil && t.Before(sixMonthsAgo) {
				dormantCount++
			}
		}
		// Not compatible if azurerm or azapi is present in the map but false
		azurermStatus, hasAzurerm := r.Compatibility["azurerm"]
		azapiStatus, hasAzapi := r.Compatibility["azapi"]
		if (hasAzurerm && !azurermStatus) || (hasAzapi && !azapiStatus) {
			notCompatibleCount++
		}
	}
	return unreachableCount, notCompatibleCount, dormantCount
}

func main() {
	app := &cli.App{
		Name:    "avm-version-check",
		Usage:   "Check Terraform Azure Verified Modules against provider version constraints",
		Version: "1.0.0",
		Commands: []*cli.Command{
			{
				Name:  "update-source",
				Usage: "Download/update the module CSV file from a remote source",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "url",
						Aliases: []string{"u"},
						Value:   "https://raw.githubusercontent.com/Azure/Azure-Verified-Modules/refs/heads/main/docs/static/module-indexes/TerraformResourceModules.csv",
						Usage:   "URL of the CSV file to download",
					},
					&cli.StringFlag{
						Name:    "output",
						Aliases: []string{"o"},
						Value:   "modules.csv",
						Usage:   "Local filename to save the CSV",
					},
					&cli.BoolFlag{
						Name:    "force",
						Aliases: []string{"f"},
						Usage:   "Force download even if the file already exists",
						Value:   false,
					},
				},
				Action: func(c *cli.Context) error {
					csvURL := c.String("url")
					csvFilename := c.String("output")
					force := c.Bool("force")
					return downloadCSVIfNeeded(csvURL, csvFilename, force)
				},
			},
			{
				Name:  "process",
				Usage: "Process a CSV file of Terraform modules, clone & check versions, output JSON results",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "input",
						Aliases: []string{"i"},
						Value:   "modules.csv",
						Usage:   "Input CSV file containing Terraform modules",
					},
					&cli.StringFlag{
						Name:    "output",
						Aliases: []string{"o"},
						Value:   "output.json",
						Usage:   "Output JSON file for results",
					},
					&cli.IntFlag{
						Name:    "workers",
						Aliases: []string{"w"},
						Value:   5,
						Usage:   "Number of concurrent workers to process records",
					},
					&cli.BoolFlag{
						Name:    "quiet",
						Aliases: []string{"q"},
						Value:   false,
						Usage:   "Suppress warning logs during processing",
					},
					&cli.BoolFlag{
						Name:    "download",
						Aliases: []string{"d"},
						Value:   false,
						Usage:   "Download the CSV file before processing (overwrites existing file)",
					},
					&cli.StringFlag{
						Name:    "url",
						Aliases: []string{"u"},
						Value:   "https://raw.githubusercontent.com/Azure/Azure-Verified-Modules/refs/heads/main/docs/static/module-indexes/TerraformResourceModules.csv",
						Usage:   "URL of the CSV file to download if --download is set",
					},
				},
				Action: func(c *cli.Context) error {
					ctx := context.Background()

					inputCSV := c.String("input")
					outputJSON := c.String("output")
					numWorkers := c.Int("workers")
					quiet := c.Bool("quiet")

					// If user requested to download the CSV, do it before reading
					if c.Bool("download") {
						url := c.String("url")
						if err := downloadCSV(url, inputCSV); err != nil {
							return fmt.Errorf("failed to download CSV before processing: %v", err)
						}
						fmt.Println("Downloaded CSV file before processing.")
					}

					// If "quiet" is true, we set the log output to discard
					if quiet {
						log.SetOutput(io.Discard)
					}

					records, err := readCSV(inputCSV)
					if err != nil {
						return fmt.Errorf("error reading CSV: %v", err)
					}

					fmt.Printf("Processing %d records using %d workers...\n", len(records), numWorkers)

					results := make([]Result, len(records))
					var index int32

					// Create progress bar
					bar := progressbar.NewOptions(len(records),
						progressbar.OptionSetDescription("Processing modules"),
						progressbar.OptionShowCount(),
						progressbar.OptionSetTheme(progressbar.Theme{
							Saucer:        "#",
							SaucerPadding: " ",
							BarStart:      "[",
							BarEnd:        "]",
						}),
					)

					recordChan := make(chan CSVRecord, len(records))
					resultChan := make(chan Result, len(records))
					var wg sync.WaitGroup

					// Start worker goroutines
					for i := 0; i < numWorkers; i++ {
						wg.Add(1)
						go func() {
							defer wg.Done()
							for record := range recordChan {
								r := processRecord(ctx, record, quiet)
								resultChan <- r

								// Update progress bar
								bar.Add(1)
							}
						}()
					}

					// Enqueue records
					for _, r := range records {
						recordChan <- r
					}
					close(recordChan)

					// Gather results
					go func() {
						wg.Wait()
						close(resultChan)
					}()

					for r := range resultChan {
						idx := atomic.AddInt32(&index, 1) - 1
						results[idx] = r
					}

					// Write results to JSON
					if err := writeJSON(outputJSON, results); err != nil {
						return fmt.Errorf("error writing output JSON: %v", err)
					}

					// Print a short summary here
					unreachableCount, notCompatibleCount, dormantCount := summarizeResults(results)
					fmt.Printf("\nProcessing complete. Results written to '%s'\n", outputJSON)
					fmt.Println("Summary:")
					fmt.Printf("  - Unreachable repos: %d\n", unreachableCount)
					fmt.Printf("  - Not-compatible repos: %d\n", notCompatibleCount)
					fmt.Printf("  - Dormant repos (6+ months): %d\n", dormantCount)

					return nil
				},
			},
			{
				Name:  "analysis",
				Usage: "Perform detailed analysis on the JSON output from 'process'",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "input",
						Aliases: []string{"i"},
						Value:   "output.json",
						Usage:   "Input JSON file with processed results",
					},
				},
				Action: func(c *cli.Context) error {
					inputJSON := c.String("input")
					data, err := os.ReadFile(inputJSON)
					if err != nil {
						return fmt.Errorf("could not read JSON file '%s': %v", inputJSON, err)
					}

					var results []Result
					if err := json.Unmarshal(data, &results); err != nil {
						return fmt.Errorf("failed to parse JSON: %v", err)
					}

					now := time.Now()
					sixMonthsAgo := now.AddDate(0, -6, 0)

					var unreachable []Result
					var dormant []Result
					var notCompatible []Result

					for _, r := range results {
						if strings.Contains(r.Error, "failed to clone repo") {
							unreachable = append(unreachable, r)
						}
						if r.LastCommitDate != "" {
							t, parseErr := time.Parse(time.RFC3339, r.LastCommitDate)
							if parseErr == nil && t.Before(sixMonthsAgo) {
								dormant = append(dormant, r)
							}
						}
						azurermStatus, hasAzurerm := r.Compatibility["azurerm"]
						azapiStatus, hasAzapi := r.Compatibility["azapi"]
						if (hasAzurerm && !azurermStatus) || (hasAzapi && !azapiStatus) {
							notCompatible = append(notCompatible, r)
						}
					}

					// Let's colorize the output with ANSI codes and add emojis
					colorRed := "\033[31m"
					colorYellow := "\033[33m"
					colorGreen := "\033[32m"
					colorCyan := "\033[36m"
					colorReset := "\033[0m"

					fmt.Printf("\n%s🔎 Detailed Analysis:%s\n", colorCyan, colorReset)
					fmt.Printf("  %sRepositories processed:%s %d\n", colorGreen, colorReset, len(results))
					fmt.Printf("  %sUnreachable repositories:%s %d\n", colorRed, colorReset, len(unreachable))
					fmt.Printf("  %sNot compatible with azurerm/azapi:%s %d\n", colorRed, colorReset, len(notCompatible))
					fmt.Printf("  %sDormant (6+ months):%s %d\n", colorYellow, colorReset, len(dormant))
					fmt.Println()

					if len(notCompatible) > 0 {
						fmt.Printf("%s❌ Not Compatible Repositories:%s\n", colorRed, colorReset)
						for _, r := range notCompatible {
							fmt.Printf("  - %s (LastCommit: %s by %s) [Owner: %s]\n",
								r.RepoURL,
								r.LastCommitDate,
								r.LastCommitAuthor,
								r.PrimaryModuleOwnerGHHandle,
							)
						}
						fmt.Println()
					}
					if len(dormant) > 0 {
						fmt.Printf("%s😴 Dormant Repositories (6+ months):%s\n", colorYellow, colorReset)
						for _, r := range dormant {
							fmt.Printf("  - %s (LastCommit: %s by %s) [Owner: %s]\n",
								r.RepoURL,
								r.LastCommitDate,
								r.LastCommitAuthor,
								r.PrimaryModuleOwnerGHHandle,
							)
						}
						fmt.Println()
					}
					if len(unreachable) > 0 {
						fmt.Printf("%s🚫 Unreachable Repositories:%s\n", colorRed, colorReset)
						for _, r := range unreachable {
							fmt.Printf("  - %s [Error: %s]\n", r.RepoURL, r.Error)
						}
						fmt.Println()
					}

					fmt.Printf("%s✅ Analysis complete.%s\n", colorGreen, colorReset)
					return nil
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
