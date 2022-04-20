namespace Tyger.Server.Logging;

public static partial class LoggerExtensions
{
    [LoggerMessage(0, LogLevel.Information, "Archived logs for run {run}")]
    public static partial void ArchivedLogsForRun(this ILogger logger, long run);

    [LoggerMessage(1, LogLevel.Information, "Retrieving archived logs for run {run}")]
    public static partial void RetrievingAchivedLogsForRun(this ILogger logger, long run);
}
