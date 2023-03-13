package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	bufferproxy "dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/buffer-proxy"
	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/cmdline"
	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/tyger"
	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/tyger/model"
	"github.com/alecthomas/units"
	"github.com/kaz-yamam0t0/go-timeparser/timeparser"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"
)

var errNotFound = errors.New("not found")

func newRunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "run",
		Aliases:               []string{"runs"},
		Short:                 "Manage runs",
		Long:                  `Manage runs`,
		DisableFlagsInUseLine: true,
		Args:                  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("a command is required")
		},
	}

	cmd.AddCommand(newRunCreateCommand())
	cmd.AddCommand(newRunExecCommand())
	cmd.AddCommand(newRunShowCommand())
	cmd.AddCommand(newRunWatchCommand())
	cmd.AddCommand(newRunLogsCommand())
	cmd.AddCommand(newRunListCommand())

	return cmd
}

func newRunCreateCommand() *cobra.Command {
	cmd := newRunCreateCommandCore("create", nil, func(r model.Run) error {
		fmt.Println(r.Id)
		return nil
	})

	cmd.Short = "Creates a run."
	cmd.Long = `Creates a run. Writes the run ID to stdout on success.`

	return cmd
}

func newRunExecCommand() *cobra.Command {
	logs := false
	logTimestamps := false

	var inputBufferParameter string
	var outputBufferParameter string

	preValidate := func(run model.Run) error {
		var resolvedCodespec model.Codespec

		if run.Job.Codespec.Inline != nil {
			resolvedCodespec = model.Codespec(*run.Job.Codespec.Inline)
		} else if run.Job.Codespec.Named != nil {
			relativeUri := fmt.Sprintf("v1/codespecs/%s", *run.Job.Codespec.Named)
			_, err := tyger.InvokeRequest(http.MethodGet, relativeUri, nil, &resolvedCodespec)
			if err != nil {
				return err
			}
		} else {
			return errors.New("a codespec for the job must be specified")
		}

		bufferParameters := resolvedCodespec.Buffers
		if bufferParameters == nil {
			return nil
		}
		switch len(bufferParameters.Inputs) {
		case 0:
			break
		case 1:
			inputBufferParameter = bufferParameters.Inputs[0]
		default:
			return errors.New("exec cannot be called if the job has multiple input buffers")
		}

		switch len(bufferParameters.Outputs) {
		case 0:
			break
		case 1:
			outputBufferParameter = bufferParameters.Outputs[0]
		default:
			return errors.New("exec cannot be called if the job has multiple output buffers")
		}

		return nil
	}

	blockSize := bufferproxy.DefaultBlockSize
	writeDop := bufferproxy.DefaultWriteDop
	readDop := bufferproxy.DefaultReadDop

	postCreate := func(run model.Run) error {

		log.Info().Int64("runId", run.Id).Msg("Run created")
		var inputSasUri string
		var outputSasUri string
		var err error
		if inputBufferParameter != "" {
			bufferId := run.Job.Buffers[inputBufferParameter]
			inputSasUri, err = getBufferAccessUri(bufferId, true)
			if err != nil {
				return err
			}
		}
		if outputBufferParameter != "" {
			bufferId := run.Job.Buffers[outputBufferParameter]
			outputSasUri, err = getBufferAccessUri(bufferId, false)
			if err != nil {
				return err
			}
		}

		wg := sync.WaitGroup{}
		if inputSasUri != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				bufferproxy.Write(inputSasUri, writeDop, blockSize, os.Stdin)
			}()
		}

		if outputSasUri != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				bufferproxy.Read(outputSasUri, readDop, os.Stdout)
			}()
		}

		if logs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := getLogs(strconv.FormatInt(run.Id, 10), logTimestamps, -1, nil, true, os.Stderr)
				if err != nil {
					log.Error().Err(err).Msg("Failed to get logs")
				}
			}()
		}

		consecutiveErrors := 0
	beginWatch:
		eventChan, errChan := watchRun(run.Id)

		for {
			select {
			case err := <-errChan:
				log.Error().Err(err).Msg("Error while watching run")
				consecutiveErrors++

				if consecutiveErrors > 1 {
					log.Fatal().Err(err).Msg("Failed to watch run")
				}

				goto beginWatch
			case event, ok := <-eventChan:
				if !ok {
					goto end
				}
				consecutiveErrors = 0

				logEntry := log.Info().Str("status", event.Status)
				if event.RunningCount != nil {
					logEntry = logEntry.Int("runningCount", *event.RunningCount)
				}
				logEntry.Msg("Run status changed")

				switch event.Status {
				case "Succeeded":
					goto end
				case "Pending":
				case "Running":
				default:
					log.Fatal().Str("status", event.Status).Str("statusReason", event.StatusReason).Msg("Run failed.")
				}
			}
		}

	end:
		wg.Wait()
		return nil
	}

	cmd := newRunCreateCommandCore("exec", preValidate, postCreate)

	blockSizeString := ""
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		cmdline.WarnIfRunningInPowerShell()

		if blockSizeString != "" {
			if blockSizeString != "" && blockSizeString[len(blockSizeString)-1] != 'B' {
				blockSizeString += "B"
			}
			parsedBlockSize, err := units.ParseBase2Bytes(blockSizeString)
			if err != nil {
				return err
			}

			blockSize = int(parsedBlockSize)
		}

		return nil
	}

	cmd.Short = "Creates a run and reads and writes to its buffers."
	cmd.Long = `Creates a run.
If the job has a single input buffer, stdin is streamed to the buffer.
If the job has a single output buffer, stdout is streamed from the buffer.`

	cmd.Flags().BoolVar(&logs, "logs", false, "Print run logs to stderr.")
	cmd.Flags().BoolVar(&logTimestamps, "timestamps", false, "Print run logs with timestamps.")

	cmd.Flags().StringVar(&blockSizeString, "block-size", blockSizeString, "Split the input stream into buffer blocks of this size.")
	cmd.Flags().IntVar(&writeDop, "write-dop", writeDop, "The degree of parallelism for writing to the input buffer.")
	cmd.Flags().IntVar(&readDop, "read-dop", readDop, "The degree of parallelism for reading from the output buffer.")

	return cmd
}

func newRunCreateCommandCore(
	commandName string,
	preValidate func(model.Run) error,
	postCreate func(model.Run) error) *cobra.Command {
	type codeTargetFlags struct {
		codespec        string
		codespecVersion string
		buffers         map[string]string
		nodePool        string
		replicas        int
	}
	var flags struct {
		specFile string
		job      codeTargetFlags
		worker   codeTargetFlags
		cluster  string
		timeout  string
	}

	getCodespecRef := func(ctf codeTargetFlags) model.CodespecRef {
		namedRef := model.NamedCodespecRef(ctf.codespec)
		if ctf.codespecVersion != "" {
			namedRef = model.NamedCodespecRef(fmt.Sprintf("%s/versions/%s", ctf.codespec, ctf.codespecVersion))
		}
		return model.CodespecRef{Named: &namedRef}
	}

	cmd := &cobra.Command{
		Use:                   fmt.Sprintf(`%s [--file YAML_SPEC] [other flags]`, commandName),
		DisableFlagsInUseLine: true,
		Args:                  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {

			newRun := model.Run{}
			if flags.specFile != "" {
				bytes, err := os.ReadFile(flags.specFile)
				if err != nil {
					return fmt.Errorf("failed to read file %s: %w", flags.specFile, err)
				}

				err = yaml.UnmarshalStrict(bytes, &newRun)
				if err != nil {
					return fmt.Errorf("failed to parse file %s: %w", flags.specFile, err)
				}

				if newRun.Job.Codespec.Inline != nil {
					newRun.Job.Codespec.Inline.Kind = "job"
				}
				if newRun.Worker != nil && newRun.Worker.Codespec.Inline != nil {
					newRun.Worker.Codespec.Inline.Kind = "worker"
				}
			}

			if flags.job.codespec != "" {
				newRun.Job.Codespec = getCodespecRef(flags.job)
			}
			if len(flags.job.buffers) > 0 {
				if newRun.Job.Buffers == nil {
					newRun.Job.Buffers = map[string]string{}
				}
				for k, v := range flags.job.buffers {
					newRun.Job.Buffers[k] = v
				}
			}
			if flags.job.nodePool != "" {
				newRun.Job.NodePool = flags.job.nodePool
			}

			if flags.job.replicas != 0 {
				newRun.Job.Replicas = flags.job.replicas
			}

			cmd.Flags().VisitAll(func(f *pflag.Flag) {
				if newRun.Worker == nil && f.Changed && strings.HasPrefix(f.Name, "worker") {
					newRun.Worker = &model.RunCodeTarget{}
				}
			})

			if newRun.Worker != nil {
				if flags.worker.codespec != "" {
					newRun.Worker.Codespec = getCodespecRef(flags.worker)
				}
				if flags.worker.nodePool != "" {
					newRun.Worker.NodePool = flags.worker.nodePool
				}
				if flags.worker.replicas != 0 {
					newRun.Worker.Replicas = flags.worker.replicas
				}
			}

			if flags.cluster != "" {
				newRun.Cluster = flags.cluster
			}

			if flags.timeout != "" {
				duration, err := time.ParseDuration(flags.timeout)
				if err != nil {
					return err
				}

				seconds := int(duration.Seconds())
				newRun.TimeoutSeconds = &seconds
			}

			if preValidate != nil {
				err := preValidate(newRun)
				if err != nil {
					return err
				}
			}

			committedRun := model.Run{}
			_, err := tyger.InvokeRequest(http.MethodPost, "v1/runs", newRun, &committedRun)
			if err != nil {
				return err
			}

			if postCreate != nil {
				err = postCreate(committedRun)
				if err != nil {
					return err
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&flags.specFile, "file", "f", "", "A YAML file with the run specification. All other flags override the values in the file.")

	cmd.Flags().StringVarP(&flags.job.codespec, "codespec", "c", "", "The name of the job codespec to execute")
	cmd.Flags().StringVar(&flags.job.codespecVersion, "version", "", "The version of the job codespec to execute")
	cmd.Flags().IntVarP(&flags.job.replicas, "replicas", "r", 1, "The number of parallel job replicas. Defaults to 1.")
	cmd.Flags().StringVar(&flags.job.nodePool, "node-pool", "", "The name of the nodepool to execute the job in")
	cmd.Flags().StringToStringVarP(&flags.job.buffers, "buffer", "b", nil, "maps a codespec buffer parameter to a buffer ID")

	cmd.Flags().StringVar(&flags.worker.codespec, "worker-codespec", "", "The name of the optional worker codespec to execute")
	cmd.Flags().StringVar(&flags.worker.codespecVersion, "worker-version", "", "The version of the optional worker codespec to execute")
	cmd.Flags().IntVar(&flags.worker.replicas, "worker-replicas", 1, "The number of parallel worker replicas. Defaults to 1 if a worker is specified.")
	cmd.Flags().StringVar(&flags.worker.nodePool, "worker-node-pool", "", "The name of the nodepool to execute the optional worker codespec in")

	cmd.Flags().StringVar(&flags.cluster, "cluster", "", "The name of the cluster to execute in")
	cmd.Flags().StringVar(&flags.timeout, "timeout", "", `How log before the run times out. Specified in a sequence of decimal numbers, each with optional fraction and a unit suffix, such as "300s", "1.5h" or "2h45m". Valid time units are "s", "m", "h"`)

	return cmd
}

func newRunShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:                   "show ID",
		Aliases:               []string{"get"},
		Short:                 "Show the details of a run",
		Long:                  `Show the details of a run.`,
		DisableFlagsInUseLine: true,
		Args:                  exactlyOneArg("run name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			run := model.Run{}
			_, err := tyger.InvokeRequest(http.MethodGet, fmt.Sprintf("v1/runs/%s", args[0]), nil, &run)
			if err != nil {
				return err
			}

			formattedRun, err := json.MarshalIndent(run, "", "  ")
			if err != nil {
				return err
			}

			fmt.Println(string(formattedRun))
			return nil
		},
	}
}

func newRunWatchCommand() *cobra.Command {
	return &cobra.Command{
		Use:                   "watch ID",
		Aliases:               []string{"get"},
		Short:                 "Watch the status changes of a run",
		Long:                  "Watch the status changes of a run",
		DisableFlagsInUseLine: true,
		Args:                  exactlyOneArg("run name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			runId, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}

			consecutiveErrors := 0
		start:
			eventChan, errChan := watchRun(runId)
			for {
				select {
				case err := <-errChan:
					if err == errNotFound {
						return errors.New("run not found")
					}

					consecutiveErrors++
					if consecutiveErrors > 1 {
						return err
					}

					log.Error().Err(err).Msg("error watching run")
					goto start
				case event, ok := <-eventChan:
					if !ok {
						return nil
					}
					consecutiveErrors = 0
					bytes, err := json.Marshal(event)
					if err != nil {
						return err
					}
					fmt.Println(string(bytes))
				}
			}
		},
	}
}

func newRunListCommand() *cobra.Command {
	var flags struct {
		limit int
		since string
	}

	cmd := &cobra.Command{
		Use:                   "list [--since DATE/TIME] [--limit COUNT]",
		Short:                 "List runs",
		Long:                  `List runs. Runs are sorted by descending created time.`,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			queryOptions := url.Values{}
			if flags.limit > 0 {
				queryOptions.Add("limit", strconv.Itoa(flags.limit))
			} else {
				flags.limit = math.MaxInt
			}
			if flags.since != "" {
				now := time.Now()
				tm, err := timeparser.ParseTimeStr(flags.since, &now)
				if err != nil {
					return fmt.Errorf("failed to parse time %s", flags.since)
				}
				queryOptions.Add("since", tm.UTC().Format(time.RFC3339Nano))
			}

			relativeUri := fmt.Sprintf("v1/runs?%s", queryOptions.Encode())
			return tyger.InvokePageRequests[model.Run](relativeUri, flags.limit, !cmd.Flags().Lookup("limit").Changed)
		},
	}

	cmd.Flags().StringVarP(&flags.since, "since", "s", "", "Results before this datetime (specified in local time) are not included")
	cmd.Flags().IntVarP(&flags.limit, "limit", "l", 1000, "The maximum number of runs to list. Default 1000")

	return cmd
}

func newRunLogsCommand() *cobra.Command {
	var flags struct {
		timestamps bool
		tailLines  int
		since      string
		follow     bool
	}

	cmd := &cobra.Command{
		Use:                   "logs RUNID",
		Short:                 "Get the logs of a run",
		Long:                  `Get the logs of a run.`,
		DisableFlagsInUseLine: true,
		Args:                  exactlyOneArg("run ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var sinceTime *time.Time
			if flags.since != "" {
				now := time.Now()
				var err error
				sinceTime, err = timeparser.ParseTimeStr(flags.since, &now)
				if err != nil {
					return fmt.Errorf("failed to parse time %s", flags.since)
				}
			}

			return getLogs(args[0], flags.timestamps, flags.tailLines, sinceTime, flags.follow, os.Stdout)
		},
	}

	cmd.Flags().BoolVar(&flags.timestamps, "timestamps", false, "Include timestamps on each line in the log output")
	cmd.Flags().IntVar(&flags.tailLines, "tail", -1, "Lines of recent log file to display")
	cmd.Flags().StringVarP(&flags.since, "since", "s", "", "Lines before this datetime (specified in local time) are not included")
	cmd.Flags().BoolVarP(&flags.follow, "follow", "f", false, "Specify if the logs should be streamed")

	return cmd
}

func getLogs(runId string, timestamps bool, tailLines int, since *time.Time, follow bool, outputSink io.Writer) error {
	queryOptions := url.Values{}
	if follow || timestamps {
		queryOptions.Add("timestamps", "true")
	}
	if tailLines >= 0 {
		queryOptions.Add("tailLines", strconv.Itoa(tailLines))
	}
	if since != nil {
		queryOptions.Add("since", since.UTC().Format(time.RFC3339Nano))
	}

	if follow {
		queryOptions.Add("follow", "true")
	}

	// If the connection drops while we are following logs, we'll try again from the last received timestamp

	for {
		resp, err := tyger.InvokeRequest(http.MethodGet, fmt.Sprintf("v1/runs/%s/logs?%s", runId, queryOptions.Encode()), nil, nil)
		if err != nil {
			return err
		}

		if !follow {
			_, err = io.Copy(outputSink, resp.Body)
			return err
		}

		lastTimestamp, err := followLogs(resp.Body, timestamps, outputSink)
		if err == nil || err == io.EOF {
			return nil
		}

		if len(lastTimestamp) > 0 {
			queryOptions.Set("since", lastTimestamp)
		}
	}
}

func followLogs(body io.Reader, includeTimestamps bool, outputSink io.Writer) (lastTimestamp string, err error) {
	reader := bufio.NewReader(body)
	atStartOfLine := true
	for {
		if atStartOfLine {
			localLastTimestamp, err := reader.ReadString(' ')
			if err != nil {
				return lastTimestamp, err
			}
			lastTimestamp = localLastTimestamp
			if includeTimestamps {
				fmt.Fprint(outputSink, lastTimestamp)
			}
		}

		line, isPrefix, err := reader.ReadLine()
		if err != nil {
			return lastTimestamp, err
		}

		atStartOfLine = !isPrefix
		if isPrefix {
			fmt.Fprint(outputSink, string(line))
		} else {
			fmt.Fprintln(outputSink, string(line))
		}
	}
}

func watchRun(runId int64) (<-chan model.RunMetadata, <-chan error) {
	runEventChan := make(chan model.RunMetadata)
	errChan := make(chan error)

	go func() {
		defer close(runEventChan)

		resp, err := tyger.InvokeRequest(http.MethodGet, fmt.Sprintf("v1/runs/%d?watch=true", runId), nil, nil)
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			errChan <- errNotFound
			return
		}
		if err != nil {
			errChan <- err
			return
		}

		if resp.StatusCode != http.StatusOK {
			errChan <- fmt.Errorf("unexpected response code %d", resp.StatusCode)
			return
		}

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err == io.EOF {
				if line != "" {
					panic("expected last line to be empty")
				}
				return
			}
			if err != nil {
				errChan <- err
				return
			}

			run := model.Run{}

			if err := json.Unmarshal([]byte(line), &run); err != nil {
				errChan <- err
				return
			}

			runEventChan <- run.RunMetadata
		}
	}()

	return runEventChan, errChan
}