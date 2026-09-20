import 'dart:io';

import 'package:test/test.dart';

/// The parity gate as a test: the Dart engine must reproduce what the Python engine did with
/// the same recorded inputs. See bin/replay.dart and tools/parity/README.md.
void main() {
  final fixtures = Directory('../tools/parity/fixtures')
      .listSync()
      .whereType<Directory>()
      .map((d) => d.path)
      .toList()
    ..sort();

  test('there is at least one recording to replay', () => expect(fixtures, isNotEmpty));

  for (final folder in fixtures) {
    test('replay matches the Python: $folder', () {
      final run = Process.runSync(Platform.resolvedExecutable, ['run', 'bin/replay.dart', folder]);
      expect(run.exitCode, 0, reason: '${run.stdout}\n${run.stderr}');
      expect(run.stdout as String, contains('PARITY OK'));
    }, timeout: const Timeout(Duration(minutes: 5)));
  }
}
