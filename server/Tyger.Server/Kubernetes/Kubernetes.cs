using System.ComponentModel.DataAnnotations;
using k8s;
using Microsoft.Extensions.Options;
using Tyger.Server.Logging;
using Tyger.Server.Model;

namespace Tyger.Server.Kubernetes;

public static class Kubernetes
{
    public static void AddKubernetes(this IServiceCollection services)
    {
        services.AddOptions<KubernetesOptions>().BindConfiguration("kubernetes").ValidateDataAnnotations().ValidateOnStart();
        services.AddSingleton<LoggingHandler>();
        services.AddSingleton(sp =>
        {
            var kubernetesOptions = sp.GetRequiredService<IOptions<KubernetesOptions>>().Value;
            var config = string.IsNullOrEmpty(kubernetesOptions.KubeconfigPath)
                ? KubernetesClientConfiguration.InClusterConfig()
                : KubernetesClientConfiguration.BuildConfigFromConfigFile(kubernetesOptions.KubeconfigPath);
            return new k8s.Kubernetes(config, sp.GetRequiredService<LoggingHandler>());
        });

        services.AddScoped<IKubernetesManager, KubernetesManager>();
        services.AddSingleton<IHostedService, KubernetesManager>();
        services.AddScoped<ILogSource, KubernetesManager>();
    }

    public static void MapClusters(this WebApplication app)
    {
        app.MapGet("/v1/clusters/", (IOptions<KubernetesOptions> config) =>
        {
            return GetClustersResponse(config.Value);
        });

        app.MapGet("/v1/clusters/{name}", (string name, IOptions<KubernetesOptions> config) =>
        {
            var cluster = GetClustersResponse(config.Value).FirstOrDefault(c => string.Equals(c.Name, name, StringComparison.OrdinalIgnoreCase));
            if (cluster == null)
            {
                return Responses.NotFound();
            }

            return Results.Ok(cluster);
        })
        .Produces<Cluster>()
        .Produces<ErrorBody>(StatusCodes.Status400BadRequest);
    }

    private static IReadOnlyList<Cluster> GetClustersResponse(KubernetesOptions options)
    {
        return options.Clusters
            .Where(c => c.Value.IsPrimary) // For now we don't support multiple clusters
            .Select(c =>
                new Cluster(
                    c.Key,
                    c.Value.Region,
                    c.Value.UserNodePools.Select(n =>
                        new NodePool(n.Key, n.Value.VmSize)).ToList()))
            .ToList();
    }
}

public class KubernetesOptions
{
    public string? KubeconfigPath { get; set; }

    [Required]
    public string Namespace { get; set; } = null!;

    [MinLength(1)]
    public Dictionary<string, ClusterOptions> Clusters { get; } = new(StringComparer.Ordinal);
}

public class ClusterOptions
{
    [Required]
    public string Region { get; init; } = null!;

    public bool IsPrimary { get; init; }

    public Dictionary<string, NodePoolOptions> UserNodePools { get; } = new(StringComparer.Ordinal);
}

public class NodePoolOptions
{
    [Required]
    public string VmSize { get; init; } = null!;
}

/// <summary>
/// Logs interactions with the Kubernetes API
/// </summary>
public class LoggingHandler : DelegatingHandler
{
    private readonly ILogger<LoggingHandler> _logger;

    public LoggingHandler(ILogger<LoggingHandler> logger)
    {
        _logger = logger;
    }

    protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        var resp = await base.SendAsync(request, cancellationToken);
        _logger.ExecutedKubernetesRequest(request.Method, request?.RequestUri?.ToString(), (int)resp.StatusCode);
        return resp;
    }
}