//go:build e2e

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/clicontext"
	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/cmd"
	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/model"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/andreyvit/diff"
	"github.com/stretchr/testify/require"
)

func init() {
	stdout, stderr, err := runTyger("login", "status")
	if err != nil {
		fmt.Fprintln(os.Stderr, stderr, stdout)
		log.Fatal(err)
	}
}

func TestEndToEnd(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	// create a codespec
	const codespecName = "testcodespec"
	digest := runCommandSuceeds(t, "docker", "inspect", "testrecon", "--format", "{{ index .RepoDigests 0 }}")

	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"-i=input", "-o=output",
		"--image", digest,
		"--",
		"-r", "$(INPUT_BUFFER_URI_FILE)", "-w", "$(OUTPUT_BUFFER_URI_FILE)")

	// create an input buffer and a SAS token to be able to write to it
	inputBufferId := runTygerSuceeds(t, "buffer", "create")
	inputSasUri := runTygerSuceeds(t, "buffer", "access", inputBufferId, "-w")

	// create and output buffer and a SAS token to be able to read from it
	outputBufferId := runTygerSuceeds(t, "buffer", "create")
	outputSasUri := runTygerSuceeds(t, "buffer", "access", outputBufferId)

	// write to the input buffer using the SAS URI
	inputContainerClient, err := azblob.NewContainerClientWithNoCredential(inputSasUri, nil)
	require.Nil(err)
	blobClient := inputContainerClient.NewBlockBlobClient("0")
	_, err = blobClient.UploadBufferToBlockBlob(context.Background(), []byte("Hello"), azblob.HighLevelUploadToBlockBlobOption{})
	require.Nil(err, err)

	// create run
	runId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m",
		"-b", fmt.Sprintf("input=%s", inputBufferId),
		"-b", fmt.Sprintf("output=%s", outputBufferId))

	waitForRunSuccess(t, runId)

	outputContainerClient, err := azblob.NewContainerClientWithNoCredential(outputSasUri, nil)
	if err != nil {
		log.Fatal(err)
	}

	outputBlockBlobClient := outputContainerClient.NewBlockBlobClient("0")
	inputResp, err := outputBlockBlobClient.Download(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}

	outputBytes, err := io.ReadAll(inputResp.Body(&azblob.RetryReaderOptions{}))
	if err != nil {
		log.Fatal(err)
	}

	require.Equal("Hello: Bonjour", string(outputBytes))
}

// Verify that a run using a codespec that requires a GPU
// is scheduled on a node with one.
func TestGpuResourceRequirement(t *testing.T) {
	t.Parallel()

	const codespecName = "gputestcodespec"
	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "nvidia/cuda:11.0-base",
		"--gpu", "1",
		"--command",
		"--",
		"bash", "-c", "[[ $(nvidia-smi -L | wc -l) == 1 ]]") // verify that a GPU is available

	// create run
	runId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m")

	waitForRunSuccess(t, runId)
}

// Verify that a run using a codespec that does not require a GPU
// is not scheduled on a node with one.
func TestNoGpuResourceRequirement(t *testing.T) {
	t.Parallel()

	const codespecName = "nogputestcodespec"
	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "nvidia/cuda:11.0-base",
		"--command",
		"--",
		"bash", "-c", "[[ ! $(nvidia-smi) ]]") // verify that no GPU is available

	// create run
	runId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m")

	waitForRunSuccess(t, runId)
}

func TestTargetGpuNodePool(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()
	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "nvidia/cuda:11.0-base",
		"--command",
		"--",
		"bash", "-c", "[[ $(nvidia-smi -L | wc -l) == 1 ]]") // verify that a GPU is available

	// create run
	runId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--node-pool", "gpunp", "--timeout", "20m")

	waitForRunSuccess(t, runId)
}

func TestTargetCpuNodePool(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()
	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "nvidia/cuda:11.0-base",
		"--command",
		"--",
		"bash", "-c", "[[ ! $(nvidia-smi) ]]") // verify that no GPU is available

	// create run
	runId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--node-pool", "cpunp", "--timeout", "10m")

	waitForRunSuccess(t, runId)
}

func TestTargetingInvalidClusterReturnsError(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()
	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "ubuntu")

	_, stderr, _ := runTyger("run", "create", "--codespec", codespecName, "--cluster", "invalid")
	require.Contains(t, stderr, "Unknown cluster")
}

func TestTargetingInvalidNodePoolReturnsError(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()
	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "ubuntu")

	_, stderr, _ := runTyger("run", "create", "--codespec", codespecName, "--node-pool", "invalid")
	require.Contains(t, stderr, "Unknown nodepool")
}

func TestTargetCpuNodePoolWithGpuResourcesReturnsError(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()
	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "ubuntu",
		"--gpu", "1")

	_, stderr, _ := runTyger("run", "create", "--codespec", codespecName, "--node-pool", "cpunp", "--timeout", "10m")
	require.Contains(t, stderr, "does not have GPUs and cannot satisfy GPU request")
}

func TestUnrecognizedFieldsRejected(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	codespec := model.Codespec{}
	requestBody := map[string]string{"image": "x"}
	_, err := cmd.InvokeRequest(http.MethodPut, "v1/codespecs/tcs", requestBody, &codespec, false)
	require.Nil(err)

	requestBody["unknownField"] = "y"
	_, err = cmd.InvokeRequest(http.MethodPut, "v1/codespecs/tcs", requestBody, &codespec, false)
	require.NotNil(err)
}

func TestResponseContainsRequestIdHeader(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	_, stderr, _ := runTyger("codespec", "show", "missing")

	require.Contains(stderr, "Request-Id")
}

func TestOpenApiSpecIsAsExpected(t *testing.T) {
	t.Parallel()
	ctx, err := clicontext.GetCliContext()
	require.Nil(t, err)
	swaggerUri := fmt.Sprintf("%s/swagger/v1/swagger.yaml", ctx.GetServerUri())
	resp, err := http.Get(swaggerUri)
	require.Nil(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	actualBytes, err := io.ReadAll(resp.Body)
	require.Nil(t, err)
	expectedFilePath, err := filepath.Abs("expected_openapi_spec.yaml")
	require.Nil(t, err)
	expectedBytes, err := ioutil.ReadFile(expectedFilePath)
	require.Nil(t, err)

	if a, e := strings.TrimSpace(string(actualBytes)), strings.TrimSpace(string(expectedBytes)); a != e {
		t.Errorf("Result not as expected. To update, run `curl %s > %s`\n\nDiff:%v",
			swaggerUri,
			expectedFilePath,
			diff.LineDiff(e, a))
	}
}

func TestListRunsPaging(t *testing.T) {
	t.Parallel()

	runTygerSuceeds(t,
		"codespec",
		"create", "exitimmediately",
		"--image", "busybox",
		"--command",
		"--",
		"echo", "hi")

	runs := make(map[string]string)
	for i := 0; i < 10; i++ {
		runs[runTygerSuceeds(t, "run", "create", "--codespec", "exitimmediately", "--timeout", "10m")] = ""
	}

	for uri := "v1/runs?limit=5"; uri != ""; {
		page := model.RunPage{}
		_, err := cmd.InvokeRequest(http.MethodGet, uri, nil, &page, false)
		require.Nil(t, err)
		for _, r := range page.Items {
			delete(runs, fmt.Sprint(r.Id))
			if len(runs) == 0 {
				return
			}
		}

		if page.NextLink == "" {
			break
		}

		uri = strings.TrimLeft(page.NextLink, "/")
	}

	require.Empty(t, runs)
}

func TestListRunsSince(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()

	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "busybox",
		"--command",
		"--",
		"echo", "hi")

	runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m")
	midId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m")
	lastId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m")

	midRunJson := runTygerSuceeds(t, "run", "show", midId)
	midRun := model.Run{}
	err := json.Unmarshal([]byte(midRunJson), &midRun)
	require.Nil(t, err)

	listJson := runTygerSuceeds(t, "run", "list", "--since", midRun.CreatedAt.Format(time.RFC3339Nano))
	list := make([]model.Run, 0)
	json.Unmarshal([]byte(listJson), &list)
	require.Greater(t, len(list), 0)
	for _, r := range list {
		require.Greater(t, r.CreatedAt.UnixNano(), midRun.CreatedAt.UnixNano())
	}

	for _, r := range list {
		if fmt.Sprint(r.Id) == lastId {
			return
		}
	}

	require.Fail(t, "last run not found")
}

func TestGetLogsFromPod(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()

	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "busybox",
		"--command",
		"--",
		"sh", "-c", "for i in `seq 1 5`; do echo $i; done; sleep 30")

	runId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m")
	t.Logf("Run ID: %s", runId)

	waitForRunStarted(t, runId)

	// block until we get the first line
	resp, err := cmd.InvokeRequest(http.MethodGet, fmt.Sprintf("v1/runs/%s/logs?follow=true", runId), nil, nil, false)
	require.Nil(t, err)
	reader := bufio.NewReader(resp.Body)
	for i := 0; i < 5; i++ {
		_, err = reader.ReadString('\n')
		require.Nil(t, err)
	}

	require.Nil(t, resp.Body.Close())

	logs := runTygerSuceeds(t, "run", "logs", runId)
	require.Equal(t, "1\n2\n3\n4\n5", logs)

	// --timestamp should prefix each line with its timestamp
	logs = runTygerSuceeds(t, "run", "logs", runId, "--timestamps")
	lines := strings.Split(logs, "\n")
	require.Equal(t, 5, len(lines))
	var firstTimestamp time.Time
	for i := len(lines) - 1; i >= 0; i-- {
		firstTimestamp, err = time.Parse(time.RFC3339Nano, strings.Split(lines[i], " ")[0])
		require.Nil(t, err)
	}

	// --since one second later. The kubernetes API appears to have a 1-second resolution when evaluating sinceTime
	logs = runTygerSuceeds(t, "run", "logs", runId, "--since", firstTimestamp.Add(time.Second).Format(time.RFC3339Nano))
	require.NotContains(t, logs, "1")

	logs = runTygerSuceeds(t, "run", "logs", runId, "--tail", "3")
	require.Equal(t, "3\n4\n5", logs)
}

func TestGetArchivedLogs(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()

	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "busybox",
		"--command",
		"--",
		"sh", "-c", "echo 1; sleep 1; echo 2; sleep 1; echo 3; sleep 1; echo 4;")

	runId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m")
	t.Logf("Run ID: %s", runId)
	waitForRunStarted(t, runId)
	logs := runTygerSuceeds(t, "run", "logs", runId, "--follow")
	require.Equal(t, "1\n2\n3\n4", logs)

	waitForRunSuccess(t, runId)

	// force logs to be archived
	_, err := cmd.InvokeRequest(http.MethodPost, "v1/runs/_sweep", nil, nil, false)
	require.Nil(t, err)

	logs = runTygerSuceeds(t, "run", "logs", runId)
	require.Equal(t, "1\n2\n3\n4", logs)

	// --timestamp should prefix each line with its timestamp
	logs = runTygerSuceeds(t, "run", "logs", runId, "--timestamps")
	lines := strings.Split(logs, "\n")
	require.Equal(t, 4, len(lines))
	var firstTimestamp time.Time
	for i := len(lines) - 1; i >= 0; i-- {
		firstTimestamp, err = time.Parse(time.RFC3339Nano, strings.Split(lines[i], " ")[0])
		require.Nil(t, err)
	}

	// --since
	logs = runTygerSuceeds(t, "run", "logs", runId, "--since", firstTimestamp.Format(time.RFC3339Nano))
	require.Equal(t, "2\n3\n4", logs)
	logs = runTygerSuceeds(t, "run", "logs", runId, "--since", firstTimestamp.Add(time.Minute).Format(time.RFC3339Nano))
	require.Equal(t, "", logs)
	logs = runTygerSuceeds(t, "run", "logs", runId, "--since", firstTimestamp.Add(-time.Minute).Format(time.RFC3339Nano))
	require.Equal(t, "1\n2\n3\n4", logs)

	// --tail
	logs = runTygerSuceeds(t, "run", "logs", runId, "--tail", "3")
	require.Equal(t, "2\n3\n4", logs)
	logs = runTygerSuceeds(t, "run", "logs", runId, "--tail", "0")
	require.Equal(t, "", logs)
	logs = runTygerSuceeds(t, "run", "logs", runId, "--tail", "4")
	require.Equal(t, "1\n2\n3\n4", logs)
}

func TestGetArchivedLogsWithLongLines(t *testing.T) {
	t.Parallel()

	codespecName := t.Name()

	runTygerSuceeds(t,
		"codespec",
		"create", codespecName,
		"--image", "busybox",
		"--command",
		"--",
		"sh", "-c", `head -c 2000000 < /dev/zero | tr '\0' 'a'; echo ""; sleep 1; head -c 2000000 < /dev/zero | tr '\0' 'b';`)

	expectedLogs := strings.Repeat("a", 2000000) + "\n" + strings.Repeat("b", 2000000)

	runId := runTygerSuceeds(t, "run", "create", "--codespec", codespecName, "--timeout", "10m")
	t.Logf("Run ID: %s", runId)
	waitForRunStarted(t, runId)
	logs := runTygerSuceeds(t, "run", "logs", runId, "--follow")
	require.Equal(t, expectedLogs, logs)

	// force logs to be archived
	_, err := cmd.InvokeRequest(http.MethodPost, "v1/runs/_sweep", nil, nil, false)
	require.Nil(t, err)

	logs = runTygerSuceeds(t, "run", "logs", runId)
	require.Equal(t, expectedLogs, logs)
}

func TestAuthenticationRequired(t *testing.T) {
	t.Parallel()
	ctx, err := clicontext.GetCliContext()
	require.Nil(t, err)
	resp, err := http.Get(fmt.Sprintf("%s/v1/runs/abc", ctx.GetServerUri()))
	require.Nil(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func runCommand(command string, args ...string) (stdout string, stderr string, err error) {

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, command, args...)

	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	err = cmd.Run()

	// strip away newline suffix
	stdout = string(bytes.TrimSuffix(outb.Bytes(), []byte{'\n'}))

	stderr = string(errb.String())
	return
}

func runCommandSuceeds(t *testing.T, command string, args ...string) string {
	stdout, stderr, err := runCommand(command, args...)
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			t.Log(stderr)
			t.Log(stdout)
			t.Errorf("Unexpected error code %d", exitError.ExitCode())
			t.FailNow()
		}
		t.Errorf("Failure executing %s: %v", command, err)
		t.FailNow()
	}

	return stdout
}

func runTyger(args ...string) (stdout string, stderr string, err error) {
	args = append([]string{"-v"}, args...)
	return runCommand("tyger", args...)
}

func runTygerSuceeds(t *testing.T, args ...string) string {
	args = append([]string{"-v"}, args...)
	return runCommandSuceeds(t, "tyger", args...)
}

func waitForRunStarted(t *testing.T, runId string) {
	start := time.Now()
	for {
		runJson := runTygerSuceeds(t, "run", "show", runId)
		run := model.Run{}
		require.Nil(t, json.Unmarshal([]byte(runJson), &run))

		switch run.Status {
		case "Pending":
		case "ContainerCreating":
			break
		default:
			return
		}

		elapsed := time.Now().Sub(start)

		switch {
		case elapsed < 10*time.Second:
			time.Sleep(time.Millisecond * 250)
		case elapsed < time.Minute:
			time.Sleep(time.Second)
		case elapsed < 15*time.Minute:
			time.Sleep(10 * time.Second)
		default:
			require.FailNowf(t, "timed out waiting for run %d.", "Run '%s'. Last status: %s", run.Id, run.Status)
		}
	}
}

func waitForRunSuccess(t *testing.T, runId string) {
	// this will block until the run terminates or we time out
	runTygerSuceeds(t, "run", "logs", runId, "-f")

	start := time.Now()
	for {
		runJson := runTygerSuceeds(t, "run", "show", runId)
		run := model.Run{}
		require.Nil(t, json.Unmarshal([]byte(runJson), &run))

		switch run.Status {
		case "Completed":
			return
		case "Pending":
		case "ContainerCreating":
		case "Running":
			break
		default:
			require.FailNowf(t, "run failed.", "Run '%s'. Last status: %s", run.Id, run.Status)
		}

		elapsed := time.Now().Sub(start)

		switch {
		case elapsed < 10*time.Second:
			time.Sleep(time.Millisecond * 250)
		case elapsed < time.Minute:
			time.Sleep(time.Second)
		case elapsed < 15*time.Minute:
			time.Sleep(10 * time.Second)
		default:
			require.FailNowf(t, "timed out waiting for run %s.", "Run '%d'. Last status: %s", run.Id, run.Status)
		}
	}
}