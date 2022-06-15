package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/clicontext"
	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/model"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/spf13/cobra"
)

func exactlyOneArg(argName string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("one %s positional argument is required", argName)
		}
		if len(args) > 1 {
			return fmt.Errorf("unexpected positional arguments after the %s: %v", argName, args[1:])
		}
		return nil
	}
}

func InvokeRequest(method string, relativeUri string, input interface{}, output interface{}, verbose bool) (*http.Response, error) {
	ctx, err := clicontext.GetCliContext()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("run 'tyger login' to connect to a Tyger server")
		}

		return nil, err
	}

	if uri, err := url.Parse(ctx.GetServerUri()); err != nil || !uri.IsAbs() {
		return nil, errors.New("run 'tyger login' to connect to a Tyger server")
	}

	token, err := ctx.GetAccessToken()
	if err != nil {
		return nil, fmt.Errorf("run 'tyger login' to connect to a Tyger server: %v", err)
	}

	absoluteUri := fmt.Sprintf("%s/%s", ctx.GetServerUri(), relativeUri)
	var body io.Reader = nil
	if input != nil {
		serializedBody, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("unable to serialize payload: %v", err)
		}
		body = bytes.NewBuffer(serializedBody)
	}

	req, err := retryablehttp.NewRequest(method, absoluteUri, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	if verbose {
		if token != "" {
			req.Header.Add("Authorization", "Bearer --REDACTED--")
		}
		if debugOutput, err := httputil.DumpRequestOut(req.Request, true); err == nil {
			fmt.Fprintln(os.Stderr, "====REQUEST=====")
			fmt.Fprintln(os.Stderr, string(debugOutput))
			fmt.Fprintln(os.Stderr, "==END REQUEST===")
			fmt.Fprintln(os.Stderr)
		}
	}

	// add the Authorization token after dumping the request so we don't write out the token
	if token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	resp, err := clicontext.NewRetryableClient().Do(req)
	if err != nil {
		return resp, fmt.Errorf("unable to connect to server: %v", err)
	}

	if verbose {
		if debugOutput, err := httputil.DumpResponse(resp, true); err == nil {
			fmt.Fprintln(os.Stderr, "====RESPONSE====")
			fmt.Fprintln(os.Stderr, string(debugOutput))
			fmt.Fprintln(os.Stderr, "==END RESPONSE==")
			fmt.Fprintln(os.Stderr)
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errorResponse := model.ErrorResponse{}
		if err = json.NewDecoder(resp.Body).Decode(&errorResponse); err == nil {
			return resp, fmt.Errorf("%s: %s", errorResponse.Error.Code, errorResponse.Error.Message)
		}

		return resp, fmt.Errorf("unexpected status code %s", resp.Status)
	}

	if output != nil {
		err = json.NewDecoder(resp.Body).Decode(output)
	}

	if err != nil {
		return resp, fmt.Errorf("unable to understand server response: %v", err)
	}

	return resp, nil
}

func InvokePageRequests(rootFlags *rootPersistentFlags, uri *string, page model.Page, queryString string, limit int, firstPage *bool, totalPrinted *int) error {

	_, err := InvokeRequest(http.MethodGet, *uri, nil, &page, rootFlags.verbose)
	if err != nil {
		return err
	}

	if *firstPage && page.GetNextLink() == "" {
		formattedRuns, err := json.MarshalIndent(page.GetItems(), "  ", "  ")
		if err != nil {
			return err
		}

		fmt.Println(string(formattedRuns))
		*uri = ""
		return nil
	}

	if *firstPage {
		fmt.Print("[\n  ")
	}

	for i, r := range page.GetItems() {
		if !*firstPage || i != 0 {
			fmt.Print(",\n  ")
		}

		formattedRun, err := json.MarshalIndent(r, "  ", "  ")
		if err != nil {
			return err
		}

		fmt.Print(string(formattedRun))
		*totalPrinted++
		if *totalPrinted == limit {
			*uri = ""
			fmt.Println("\n]")
			return nil
		}
	}

	*firstPage = false
	*uri = strings.TrimLeft(page.GetNextLink(), "/")
	if *uri == "" {
		fmt.Println("\n]")
	}
	return nil
}
