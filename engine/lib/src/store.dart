/// Where the engine gets the time and keeps its files. Both are handed in, so the engine can
/// be replayed against a recording, run with no disk, or pointed at a phone's app folder.
library;

/// The engine's view of time. The Python reads the wall clock from inside the engine, which
/// is why it needs patching to be replayed; here the clock is a parameter.
abstract class Clock {
  /// Unix time in seconds.
  double now();

  /// Seconds from some fixed point; only differences mean anything.
  double monotonic();
}

class SystemClock implements Clock {
  final _stopwatch = Stopwatch()..start();

  @override
  double now() => DateTime.now().microsecondsSinceEpoch / 1e6;

  @override
  double monotonic() => _stopwatch.elapsedMicroseconds / 1e6;
}

/// A clock that stands where it is put. For replays and tests.
class ManualClock implements Clock {
  double t;
  ManualClock([this.t = 0.0]);

  @override
  double now() => t;

  @override
  double monotonic() => t;
}

/// One coin's saved state and trade log.
abstract class Store {
  /// The last saved state, or null if there is none (or it can't be read).
  Map<String, dynamic>? loadState();

  void saveState(Map<String, dynamic> state);

  /// Append one row to the trade log. [header] is written first if the log is new.
  void appendTrade(List<String> header, List<String> row);

  /// Set the current files aside under a name carrying [stamp], so a reset loses nothing.
  void archive(String stamp);
}

/// Keeps everything in memory.
class MemoryStore implements Store {
  Map<String, dynamic>? state;
  final List<List<String>> trades = [];

  @override
  Map<String, dynamic>? loadState() => state;

  @override
  void saveState(Map<String, dynamic> state) => this.state = state;

  @override
  void appendTrade(List<String> header, List<String> row) {
    if (trades.isEmpty) trades.add(header);
    trades.add(row);
  }

  @override
  void archive(String stamp) {
    state = null;
    trades.clear();
  }
}

/// One CSV line as Python's csv module writes it: quoted only where needed, no line ending.
String csvLine(List<String> fields) => fields.map((f) {
      if (f.contains(',') || f.contains('"') || f.contains('\n') || f.contains('\r')) {
        return '"${f.replaceAll('"', '""')}"';
      }
      return f;
    }).join(',');
