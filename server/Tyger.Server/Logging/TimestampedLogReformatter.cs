using System.Buffers;
using System.Diagnostics;
using System.IO.Pipelines;

namespace Tyger.Server.Logging;

/// <summary>
/// Works around a quirk with Kubernetes log formatting.
/// When requesting logs with each line prefixed with its timestamp, the format looks like this:
/// 2022-04-14T16:22:18.803090288Z This is my message
///
/// But when a single message (line) is very long, it ends up with timestamps interspersed within it.
/// The format then end up being
/// (timestamp) (16K of message)(timestamp) (16K of message)(timestamp)...
///
/// Here we reformat those logs and to take out those extra timestamps from the messages.
///
/// Another issue is that sometimes the log content will not start with a timestamp. This can happen
/// when there is a problem retrieving the logs but the HTTP response is 200. The log body
/// will then be something like "unable to retrieve container logs for containerd://...". In this case,
/// we prepend a dummy timestamp of 0001-01-01T00:00:00.000000000Z.
/// </summary>
public class TimestampedLogReformatter : IPipelineElement
{
    public const int LineBlockSize = 0x4000;
    private static readonly Memory<byte> s_emptyTimestampPrefix = System.Text.Encoding.UTF8.GetBytes("0001-01-01T00:00:00.000000000Z ");

    public async Task Process(PipeReader reader, PipeWriter writer, CancellationToken cancellationToken)
    {
        long remainingBytesLeftInMessageBlock = 0;
        bool discardNextDate = false;
        while (true)
        {
            var result = await reader.ReadAsync(cancellationToken);
            var buffer = result.Buffer;

            SequencePosition consumedPosition = ProcessBuffer(buffer, writer, ref remainingBytesLeftInMessageBlock, ref discardNextDate);

            await writer.FlushAsync(cancellationToken);

            if (result.IsCompleted)
            {
                break;
            }

            reader.AdvanceTo(consumedPosition, buffer.End);
        }
    }

    private static SequencePosition ProcessBuffer(in ReadOnlySequence<byte> sequence, PipeWriter writer, ref long remainingBytesLeftInMessageBlock, ref bool discardNextDate)
    {
        var reader = new SequenceReader<byte>(sequence);
        while (reader.Remaining > 0)
        {
            if (remainingBytesLeftInMessageBlock == 0)
            {
                // expecting to be positioned at a date or the end
                var timestampStartPosition = reader.Position;
                if (!reader.TryAdvanceTo((byte)' ', true))
                {
                    return reader.Position;
                }

                if (!discardNextDate)
                {
                    var tsSequence = sequence.Slice(timestampStartPosition, reader.Position);
                    if (!TimestampParser.TryParseTimestampFromSequence(tsSequence, out _))
                    {
                        writer.Write(s_emptyTimestampPrefix.Span);
                    }

                    foreach (var segment in tsSequence)
                    {
                        writer.Write(segment.Span);
                    }
                }

                discardNextDate = true;
                remainingBytesLeftInMessageBlock = LineBlockSize;
            }

            Debug.Assert(remainingBytesLeftInMessageBlock <= LineBlockSize);

            var startPosition = reader.Position;
            var startConsumed = reader.Consumed;

            if (reader.TryAdvanceTo((byte)'\n', true))
            {
                remainingBytesLeftInMessageBlock -= reader.Consumed - startConsumed;
                if (remainingBytesLeftInMessageBlock < 0)
                {
                    reader.Rewind(-remainingBytesLeftInMessageBlock);
                }
                else
                {
                    discardNextDate = false;
                }

                remainingBytesLeftInMessageBlock = 0;
            }
            else
            {
                reader.Advance(Math.Min(reader.Remaining, remainingBytesLeftInMessageBlock));
                remainingBytesLeftInMessageBlock -= reader.Consumed - startConsumed;
            }

            foreach (var segment in sequence.Slice(startPosition, reader.Position))
            {
                writer.Write(segment.Span);
            }
        }

        return reader.Position;
    }
}
