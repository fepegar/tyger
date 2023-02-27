using System.Collections.Immutable;
using System.ComponentModel.DataAnnotations;
using System.Globalization;
using System.Text;
using System.Text.Json;
using k8s;
using k8s.Models;
using Microsoft.Extensions.Options;
using Tyger.Server.Buffers;
using Tyger.Server.Database;
using Tyger.Server.Model;
using Tyger.Server.StorageServer;
using static Tyger.Server.Kubernetes.KubernetesMetadata;

namespace Tyger.Server.Kubernetes;

public class RunCreator
{
    private readonly IKubernetes _client;
    private readonly IRepository _repository;
    private readonly BufferManager _bufferManager;
    private readonly BufferOptions _bufferOptions;
    private readonly KubernetesOptions _k8sOptions;
    private readonly StorageServerOptions _storageServerOptions;
    private readonly ILogger<RunCreator> _logger;

    public RunCreator(
        IKubernetes client,
        IRepository repository,
        BufferManager bufferManager,
        IOptions<KubernetesOptions> k8sOptions,
        IOptions<StorageServerOptions> storageServerOptions,
        IOptions<BufferOptions> bufferOptions,
        ILogger<RunCreator> logger)
    {
        _client = client;
        _repository = repository;
        _bufferManager = bufferManager;
        _bufferOptions = bufferOptions.Value;
        _k8sOptions = k8sOptions.Value;
        _storageServerOptions = storageServerOptions.Value;
        _logger = logger;
    }

    public async Task<Run> CreateRun(NewRun newRun, CancellationToken cancellationToken)
    {
        // Phase 1: Validate newRun and create the leaf building blocks.

        ClusterOptions targetCluster = GetTargetCluster(newRun);
        var jobCodespec = await GetCodespec(newRun.Job.Codespec, cancellationToken);
        newRun = newRun with { Cluster = targetCluster.Name, Job = newRun.Job with { Codespec = jobCodespec.NormalizedRef() } };
        var jobPodTemplateSpec = CreatePodTemplateSpec(jobCodespec, newRun.Job, targetCluster, "Never");

        var bufferMap = await GetBufferMap(jobCodespec.Buffers, newRun.Job.Buffers, cancellationToken);

        V1PodTemplateSpec? workerPodTemplateSpec = null;
        Codespec? workerCodespec = null;
        if (newRun.Worker != null)
        {
            workerCodespec = await GetCodespec(newRun.Worker.Codespec, cancellationToken);
            newRun = newRun with { Worker = newRun.Worker with { Codespec = workerCodespec.NormalizedRef() } };
            workerPodTemplateSpec = CreatePodTemplateSpec(workerCodespec, newRun.Worker, targetCluster, "Always");
        }

        // Phase 2: now that we have performed validation, create a record for this run in the database

        var run = await _repository.CreateRun(newRun, cancellationToken);

        // Phase 3: assemble and create Kubernetes objects

        var commonLabels = ImmutableDictionary<string, string>.Empty.Add(RunLabel, $"{run.Id}");

        var jobLabels = commonLabels.Add(JobLabel, $"{run.Id}");
        jobPodTemplateSpec.Metadata.Labels = jobLabels;

        var job = new V1Job
        {
            Metadata = new()
            {
                Name = JobNameFromRunId(run.Id),
                Labels = jobLabels
            },
            Spec = new()
            {
                Parallelism = run.Job.Replicas,
                Completions = run.Job.Replicas,
                CompletionMode = "Indexed",
                ManualSelector = true,
                Selector = new() { MatchLabels = jobLabels },
                Template = jobPodTemplateSpec,
                ActiveDeadlineSeconds = run.TimeoutSeconds,
                BackoffLimit = 0,
            },
        };

        if (bufferMap != null)
        {
            await AddBufferProxySidecars(job, run, bufferMap, cancellationToken);
        }

        if (newRun.Worker != null)
        {
            var workerLabels = commonLabels.Add(WorkerLabel, $"{run.Id}");
            workerPodTemplateSpec!.Metadata.Labels = workerLabels;

            var workerStatefulSet = new V1StatefulSet()
            {
                Metadata = new()
                {
                    Name = StatefulSetNameFromRunId(run.Id),
                    Labels = workerLabels
                },
                Spec = new()
                {
                    PodManagementPolicy = "Parallel",
                    Replicas = newRun.Worker.Replicas,
                    Template = workerPodTemplateSpec,
                    Selector = new() { MatchLabels = workerLabels },
                    ServiceName = StatefulSetNameFromRunId(run.Id)
                },
            };

            AddWaitForWorkerInitContainersToJob(job, run);
            AddWorkerNodesEnvironmentVariables(job, run, workerCodespec);

            await _client.AppsV1.CreateNamespacedStatefulSetAsync(workerStatefulSet, _k8sOptions.Namespace, cancellationToken: cancellationToken);

            var headlessWorkerService = new V1Service
            {
                Metadata = new()
                {
                    Name = StatefulSetNameFromRunId(run.Id),
                    Labels = workerLabels,
                },
                Spec = new()
                {
                    ClusterIP = "None",
                    Selector = workerLabels,
                }
            };

            await _client.CoreV1.CreateNamespacedServiceAsync(headlessWorkerService, _k8sOptions.Namespace, cancellationToken: cancellationToken);
        }

        await _client.BatchV1.CreateNamespacedJobAsync(job, _k8sOptions.Namespace, cancellationToken: cancellationToken);

        // Phase 4: Inform the database that the Kubernetes objects have been created in the cluster.

        await _repository.UpdateRun(run, resourcesCreated: true, cancellationToken: cancellationToken);
        _logger.CreatedRun(run.Id);
        return run;
    }

    private void AddWaitForWorkerInitContainersToJob(V1Job job, Run run)
    {
        var initContainers = job.Spec.Template.Spec.InitContainers ??= new List<V1Container>();

        initContainers.Add(
            new()
            {
                Name = "imagepull",
                Image = GetMainContainer(job.Spec.Template.Spec).Image,
                Command = new[] { "/no-op/no-op" },
                VolumeMounts = new V1VolumeMount[] { new("/no-op/", "no-op") }
            });

        var waitScript = new StringBuilder("set -euo pipefail").AppendLine();
        waitScript.AppendLine($"until kubectl wait --for=condition=ready pod -l {WorkerLabel}={run.Id}; do echo waiting for workers to be ready; sleep 1; done;");
        foreach (var host in GetWorkerDnsNames(run))
        {
            waitScript.AppendLine($"until nslookup {host}; do echo waiting for hostname {host} to resolve; sleep 1; done;");
        }

        initContainers.Add(
            new()
            {
                Name = "waitforworker",
                Image = _k8sOptions.WorkerWaiterImage,
                Command = new[] { "bash", "-c", waitScript.ToString() },
            });

        (job.Spec.Template.Spec.Volumes ??= new List<V1Volume>()).Add(new()
        {
            Name = "no-op",
            ConfigMap = new V1ConfigMapVolumeSource
            {
                DefaultMode = 111,
                Name = _k8sOptions.NoOpConfigMap
            }
        });
        job.Spec.Template.Spec.ServiceAccountName = _k8sOptions.JobServiceAccount;
    }

    private void AddWorkerNodesEnvironmentVariables(V1Job job, Run run, Codespec? workerCodespec)
    {
        var dnsNames = GetWorkerDnsNames(run);

        var envVars = GetMainContainer(job.Spec.Template.Spec).Env ??= new List<V1EnvVar>();
        envVars.Add(new("TYGER_WORKER_NODES", JsonSerializer.Serialize(dnsNames)));
        if (workerCodespec?.Endpoints != null)
        {
            foreach ((var name, var port) in workerCodespec.Endpoints)
            {
                envVars.Add(new($"TYGER_{name.ToUpperInvariant()}_WORKER_ENDPOINT_ADDRESSES", JsonSerializer.Serialize(dnsNames.Select(n => $"{n}:{port}"))));
            }
        }
    }

    private string[] GetWorkerDnsNames(Run run)
    {
        return Enumerable.Range(0, run.Worker!.Replicas).Select(i => $"{StatefulSetNameFromRunId(run.Id)}-{i}.{StatefulSetNameFromRunId(run.Id)}.{_k8sOptions.Namespace}.svc.cluster.local").ToArray();
    }

    private async Task AddBufferProxySidecars(V1Job job, Run run, Dictionary<string, (bool write, Uri sasUri)> bufferMap, CancellationToken cancellationToken)
    {
        const string SecretMountPath = "/etc/buffer-sas-tokens";
        const string FifoMountPath = "/etc/buffer-fifos";
        const string PipeVolumeName = "pipevolume";

        var mainContainer = GetMainContainer(job.Spec.Template.Spec);
        foreach (var envVar in bufferMap.Select(p => new V1EnvVar($"{p.Key.ToUpperInvariant()}_PIPE", $"{FifoMountPath}/{p.Key}")))
        {
            mainContainer.Env.Add(envVar);
        }

        var buffersSecret = new V1Secret
        {
            Metadata = new()
            {
                Name = SecretNameFromRunId(run.Id),
                Labels = job.Labels() ?? throw new InvalidOperationException("expected job labels to be set"),
            },
            StringData = bufferMap.ToDictionary(p => p.Key, p => p.Value.sasUri.ToString()),
        };

        (job.Spec.Template.Spec.Volumes ??= new List<V1Volume>()).Add(
            new()
            {
                Name = "buffers",
                Secret = new() { SecretName = buffersSecret.Metadata.Name },
            });

        job.Spec.Template.Spec.Volumes.Add(new() { Name = PipeVolumeName, EmptyDir = new() });

        var fifoVolumeMount = new V1VolumeMount(FifoMountPath, PipeVolumeName);
        (mainContainer.VolumeMounts ??= new List<V1VolumeMount>()).Add(fifoVolumeMount);

        var mkfifoBuilder = new StringBuilder("set -euo pipefail").AppendLine();
        foreach (var buffer in bufferMap)
        {
            var fifoPath = $"{FifoMountPath}/{buffer.Key}";
            mkfifoBuilder.AppendLine($"mkfifo {fifoPath}").AppendLine($"chmod 666 {fifoPath}");
        }

        (job.Spec.Template.Spec.InitContainers ??= new List<V1Container>()).Add(
            new()
            {
                Name = "mkfifo",
                Image = "mcr.microsoft.com/cbl-mariner/base/core:2.0-nonroot",
                Command = new[] { "bash", "-c", mkfifoBuilder.ToString() },
                VolumeMounts = new[] { fifoVolumeMount }
            }
        );

        foreach ((string bufferName, (bool write, Uri sasUri)) in bufferMap)
        {
            job.Spec.Template.Spec.Containers.Add(new()
            {
                Name = $"{bufferName}-buffer-proxy",
                Image = _bufferOptions.BufferProxyImage,
                Args = new[]
                {
                    write ? "write" : "read",
                    $"{SecretMountPath}/{bufferName}",
                    write ? "--input" : "--output",
                    $"{FifoMountPath}/{bufferName}",
                    "--log-format", "json",
                },
                VolumeMounts = new[]
                {
                    fifoVolumeMount,
                    new()
                    {
                        Name = "buffers",
                        MountPath = SecretMountPath,
                        ReadOnlyProperty = true,

                    },
                },
            });
        }

        await _client.CoreV1.CreateNamespacedSecretAsync(buffersSecret, _k8sOptions.Namespace, cancellationToken: cancellationToken);
        _logger.CreatedSecret(buffersSecret.Metadata.Name);
    }

    private static V1Container GetMainContainer(V1PodSpec podSpec) => podSpec.Containers.Single(c => c.Name == "main");

    private ClusterOptions GetTargetCluster(NewRun newRun)
    {
        ClusterOptions? targetCluster;
        if (!string.IsNullOrEmpty(newRun.Cluster))
        {
            if (!_k8sOptions.Clusters.TryGetValue(newRun.Cluster, out var cluster))
            {
                throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "Unknown cluster '{0}'", newRun.Cluster));
            }

            targetCluster = cluster;
        }
        else
        {
            // Only supporting single cluster for the moment
            targetCluster = _k8sOptions.Clusters.Values.First();
        }

        return targetCluster;
    }

    private V1PodTemplateSpec CreatePodTemplateSpec(Codespec codespec, RunCodeTarget codeTarget, ClusterOptions? targetCluster, string restartPolicy)
    {
        var podTemplateSpec = new V1PodTemplateSpec()
        {
            Metadata = new()
            {
                Finalizers = new[] { FinalizerName }
            },
            Spec = new()
            {
                Containers = new List<V1Container>()
                {
                    new()
                    {
                        Name = "main",
                        Image = codespec.Image,
                        Command = codespec.Command,
                        Args = codespec.Args,
                        Env = codespec.Env?.Select(p => new V1EnvVar(p.Key, p.Value)).ToList()
                    }
                },
                RestartPolicy = restartPolicy,
            }
        };

        AddComputeResources(podTemplateSpec, codespec, codeTarget, targetCluster);
        AddStorageServer(podTemplateSpec);

        return podTemplateSpec;
    }

    private void AddStorageServer(V1PodTemplateSpec podTemplateSpec)
    {
        (GetMainContainer(podTemplateSpec.Spec).Env ??= new List<V1EnvVar>()).Add(new("MRD_STORAGE_URI", _storageServerOptions.Uri));
    }

    private static void AddComputeResources(V1PodTemplateSpec podTemplateSpec, Codespec codespec, RunCodeTarget codeTarget, ClusterOptions? targetCluster)
    {
        string? targetNodePool = null;
        bool targetsGpuNodePool = false;
        if (!string.IsNullOrEmpty(codeTarget.NodePool))
        {
            if (targetCluster == null)
            {
                throw new ValidationException("A cluster must be specified if a nodepool is specified.");
            }

            targetNodePool = codeTarget.NodePool;
            if (!targetCluster.UserNodePools.TryGetValue(codeTarget.NodePool, out var pool))
            {
                throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "Unknown nodepool '{0}'", targetNodePool));
            }

            targetsGpuNodePool = DoesVmHaveSupportedGpu(pool.VmSize);

            if (!targetsGpuNodePool && codespec.Resources?.Gpu is ResourceQuantity q && q.ToDecimal() != 0)
            {
                throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "Nodepool '{0}' does not have GPUs and cannot satisfy GPU request '{1}'", targetNodePool, q));
            }
        }

        if (codespec.Resources != null)
        {
            Dictionary<string, ResourceQuantity> requests = ToDictionary(codespec.Resources.Requests);
            Dictionary<string, ResourceQuantity> limits = ToDictionary(codespec.Resources.Limits);

            if (codespec.Resources.Gpu != null)
            {
                requests["nvidia.com/gpu"] = limits["nvidia.com/gpu"] = codespec.Resources.Gpu;
            }

            GetMainContainer(podTemplateSpec.Spec).Resources = new() { Requests = requests, Limits = limits };
        }

        podTemplateSpec.Spec.Tolerations = new List<V1Toleration>
            {
                new() { Key = "tyger", OperatorProperty= "Equal", Value = "run", Effect = "NoSchedule" } // allow this to run on a user nodepools
            };
        if (codespec.Resources?.Gpu != null || targetsGpuNodePool)
        {
            podTemplateSpec.Spec.Tolerations.Add(new() { Key = "sku", OperatorProperty = "Equal", Value = "gpu", Effect = "NoSchedule" });
        }

        podTemplateSpec.Spec.NodeSelector = new Dictionary<string, string> { { "tyger", "run" } }; // require this to run on a user nodepool
        if (targetNodePool != null)
        {
            podTemplateSpec.Spec.NodeSelector.Add("agentpool", targetNodePool);
        }
    }

    private static Dictionary<string, ResourceQuantity> ToDictionary(OvercommittableResources? resources)
    {
        var dict = new Dictionary<string, ResourceQuantity>();
        if (resources?.Cpu != null)
        {
            dict["cpu"] = resources.Cpu;
        }

        if (resources?.Memory != null)
        {
            dict["memory"] = resources.Memory;
        }

        return dict;
    }

    private static bool DoesVmHaveSupportedGpu(string vmSize)
    {
        return vmSize.StartsWith("Standard_N", StringComparison.OrdinalIgnoreCase) &&
            !vmSize.EndsWith("_v4", StringComparison.OrdinalIgnoreCase); // unsupported AMD GPU
    }

    private async Task<Codespec> GetCodespec(string codespecRef, CancellationToken cancellationToken)
    {
        var codespecTokens = codespecRef.Split("/versions/");
        Codespec? codespec;
        int version;

        switch (codespecTokens.Length)
        {
            case 1:
                codespec = await _repository.GetLatestCodespec(codespecTokens[0], cancellationToken);
                if (codespec == null)
                {
                    throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "The codespec '{0}' was not found", codespecTokens[0]));
                }

                break;
            case 2:
                if (!int.TryParse(codespecTokens[1], out version))
                {
                    throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "'{0}' is not a valid codespec version", codespecTokens[1]));
                }

                codespec = await _repository.GetCodespecAtVersion(codespecTokens[0], version, cancellationToken);
                if (codespec != null)
                {
                    break;
                }

                // See if it's just the version number that was not found
                var latestCodespec = await _repository.GetLatestCodespec(codespecTokens[0], cancellationToken);
                if (latestCodespec == null)
                {
                    throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "The codespec '{0}' was not found", codespecTokens[0]));
                }

                throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "The version '{0}' of codespec '{1}' was not found. The latest version is '{2}'.", version, codespecTokens[0], latestCodespec.Version));

            default:
                throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "The codespec '{0}' is invalid. The value should be in the form '<codespec_name>' or '<codespec_name>/versions/<version_number>'.", codespecRef));
        }

        return codespec;
    }

    private async Task<Dictionary<string, (bool write, Uri sasUri)>> GetBufferMap(BufferParameters? parameters, Dictionary<string, string>? arguments, CancellationToken cancellationToken)
    {
        arguments = arguments == null ? new(StringComparer.OrdinalIgnoreCase) : new(arguments, StringComparer.OrdinalIgnoreCase);
        IEnumerable<(string param, bool writeable)> combinedParameters = (parameters?.Inputs?.Select(param => (param, false)) ?? Enumerable.Empty<(string, bool)>())
            .Concat(parameters?.Outputs?.Select(param => (param, true)) ?? Enumerable.Empty<(string, bool)>());

        var outputMap = new Dictionary<string, (bool write, Uri sasUri)>();

        foreach (var param in combinedParameters)
        {
            if (!arguments.TryGetValue(param.param, out var bufferId))
            {
                throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "The run is missing required buffer argument '{0}'", param.param));
            }

            var bufferAccess = await _bufferManager.CreateBufferAccessString(bufferId, param.writeable, cancellationToken);
            if (bufferAccess == null)
            {
                throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "The buffer '{0}' was not found", bufferId));
            }

            outputMap[param.param] = (param.writeable, bufferAccess.Uri);
            arguments.Remove(param.param);
        }

        foreach (var arg in arguments)
        {
            throw new ValidationException(string.Format(CultureInfo.InvariantCulture, "Buffer argument '{0}' does not correspond to a buffer parameter on the codespec", arg));
        }

        return outputMap;
    }
}
