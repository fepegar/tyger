package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"dev.azure.com/msresearch/compimag/_git/tyger/cli/internal/model"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"
)

func newCodespecCommand(rootFlags *rootPersistentFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "codespec",
		Aliases:               []string{"codespecs"},
		Short:                 "Manage codespecs",
		Long:                  `Manage codespecs`,
		DisableFlagsInUseLine: true,
		Args:                  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("a command is required")
		},
	}

	cmd.AddCommand(newCodespecCreateCommand(rootFlags))
	cmd.AddCommand(newCodespecShowCommand(rootFlags))
	cmd.AddCommand(codespecListCommand(rootFlags))

	return cmd
}

func newCodespecCreateCommand(rootFlags *rootPersistentFlags) *cobra.Command {
	var flags struct {
		image         string
		kind          string
		inputBuffers  []string
		outputBuffers []string
		env           map[string]string
		command       bool
		cpu           string
		memory        string
		gpu           string
		maxReplicas   int
	}

	var cmd = &cobra.Command{
		Use:                   "create NAME --image IMAGE [--kind job|worker] [--max-replicas REPLICAS] [[--input BUFFER_NAME] ...] [[--output BUFFER_NAME] ...] [[--env \"KEY=VALUE\"] ...] [resources] [--command] -- [COMMAND] [args...]",
		Short:                 "Create or update a codespec",
		Long:                  `Create or update a codespec. Outputs the version of the codespec that was created.`,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || cmd.ArgsLenAtDash() == 0 {
				return errors.New("a name for the codespec is required")
			}
			if len(args) > 1 && cmd.ArgsLenAtDash() == -1 {
				return fmt.Errorf("unexpected positional args %v. Container arguments must be preceded by -- and placed at the end of the command-line", args[1:])
			}
			if len(args) > 1 && cmd.ArgsLenAtDash() > 1 {
				unexpectedArgs := args[1:cmd.ArgsLenAtDash()]
				return fmt.Errorf("unexpected positional args before container args: %v", unexpectedArgs)
			}

			codespecName := args[0]
			containerArgs := args[1:]

			var isValidName = regexp.MustCompile(`^[a-z0-9\-._]*$`).MatchString
			if !isValidName(codespecName) {
				return errors.New("codespec names must contain only lower case letters (a-z), numbers (0-9), dashes (-), underscores (_), and dots (.)")
			}

			flags.kind = strings.ToLower(flags.kind)

			switch flags.kind {
			case "job":
			case "worker":
				break
			default:
				return errors.New("--kind must be either 'job' or worker'")
			}

			newCodespec := model.NewCodespec{
				Kind:  flags.kind,
				Image: flags.image,
				Buffers: &model.BufferParameters{
					Inputs:  flags.inputBuffers,
					Outputs: flags.outputBuffers,
				},
				Env:         flags.env,
				Resources:   &model.CodespecResources{},
				MaxReplicas: flags.maxReplicas,
			}

			if flags.command {
				newCodespec.Command = containerArgs
			} else {
				newCodespec.Args = containerArgs
			}

			if (flags.cpu) != "" {
				_, err := resource.ParseQuantity(flags.cpu)
				if err != nil {
					return fmt.Errorf("cpu value is invalid: %v", err)
				}

				newCodespec.Resources.Cpu = &flags.cpu
			}
			if (flags.memory) != "" {
				_, err := resource.ParseQuantity(flags.memory)
				if err != nil {
					return fmt.Errorf("memory value is invalid: %v", err)
				}

				newCodespec.Resources.Memory = &flags.memory
			}
			if (flags.gpu) != "" {
				_, err := resource.ParseQuantity(flags.gpu)
				if err != nil {
					return fmt.Errorf("gpu value is invalid: %v", err)
				}

				newCodespec.Resources.Gpu = &flags.gpu
			}

			resp, err := InvokeRequest(http.MethodPut, fmt.Sprintf("v1/codespecs/%s", codespecName), newCodespec, &newCodespec, rootFlags.verbose)
			if err != nil {
				return err
			}

			version, err := getCodespecVersionFromResponse(resp)
			if err != nil {
				return fmt.Errorf("unable to get codespec version: %v", err)
			}
			fmt.Println(version)

			return nil
		},
	}

	cmd.Flags().StringVar(&flags.image, "image", "", "The container image (required)")
	if err := cmd.MarkFlagRequired("image"); err != nil {
		log.Panicln(err)
	}
	cmd.Flags().StringVarP(&flags.kind, "kind", "k", "job", "The codespec kind. Either 'job' (the default) or 'worker'.")
	cmd.Flags().IntVarP(&flags.maxReplicas, "max-replicas", "r", 1, "The maximum number of replicas this codespec supports.")
	cmd.Flags().StringSliceVarP(&flags.inputBuffers, "input", "i", nil, "Input buffer parameter names")
	cmd.Flags().StringSliceVarP(&flags.outputBuffers, "output", "o", nil, "Output buffer parameter names")
	cmd.Flags().StringToStringVarP(&flags.env, "env", "e", nil, "Environment variables to set in the container in the form KEY=value")
	cmd.Flags().BoolVar(&flags.command, "command", false, "If true and extra arguments are present, use them as the 'command' field in the container, rather than the 'args' field which is the default.")
	cmd.Flags().StringVarP(&flags.cpu, "cpu", "c", "", "CPU cores needed")
	cmd.Flags().StringVarP(&flags.memory, "memory", "m", "", "memory bytes needed")
	cmd.Flags().StringVarP(&flags.gpu, "gpu", "g", "", "GPUs needed")

	return cmd
}

func newCodespecShowCommand(rootFlags *rootPersistentFlags) *cobra.Command {
	var flags struct {
		version int
	}

	var cmd = &cobra.Command{
		Use:                   "show NAME [--version VERSION]",
		Aliases:               []string{"get"},
		Short:                 "Show the details of a codespec",
		Long:                  `Show the details of a codespec.`,
		DisableFlagsInUseLine: true,
		Args:                  exactlyOneArg("codespec name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			relativeUri := fmt.Sprintf("v1/codespecs/%s", name)
			var version *int
			if cmd.Flag("version").Changed {
				version = &flags.version
				relativeUri = fmt.Sprintf("%s/versions/%d", relativeUri, *version)
			}

			codespec := model.Codespec{}
			_, err := InvokeRequest(http.MethodGet, relativeUri, nil, &codespec, rootFlags.verbose)
			if err != nil {
				return err
			}

			formattedRun, err := json.MarshalIndent(codespec, "  ", "  ")
			if err != nil {
				return err
			}

			fmt.Println(string(formattedRun))
			return nil
		},
	}

	cmd.Flags().IntVar(&flags.version, "version", -1, "the version of the codespec to get")

	return cmd
}

func getCodespecVersionFromResponse(resp *http.Response) (int, error) {
	location := resp.Header.Get("Location")
	return strconv.Atoi(location[strings.LastIndex(location, "/")+1:])
}

func codespecListCommand(rootFlags *rootPersistentFlags) *cobra.Command {
	var flags struct {
		limit  int
		prefix string
	}

	cmd := &cobra.Command{
		Use:                   "list [--prefix STRING] [--limit COUNT]",
		Short:                 "List codespecs",
		Long:                  `List codespecs. Latest version of codespecs are sorted alphabetically.`,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			queryOptions := url.Values{}
			if flags.limit > 0 {
				queryOptions.Add("limit", strconv.Itoa(flags.limit))
			} else {
				flags.limit = math.MaxInt
			}
			if flags.prefix != "" {
				queryOptions.Add("prefix", flags.prefix)
			}

			var relativeUri string = fmt.Sprintf("v1/codespecs?%s", queryOptions.Encode())
			return InvokePageRequests[model.Codespec](rootFlags, relativeUri, flags.limit, !cmd.Flags().Lookup("limit").Changed)
		},
	}

	cmd.Flags().StringVarP(&flags.prefix, "prefix", "p", "", "Show only codespecs that start with this prefix")
	cmd.Flags().IntVarP(&flags.limit, "limit", "l", 1000, "The maximum number of codespecs to list. Default 1000")

	return cmd
}
