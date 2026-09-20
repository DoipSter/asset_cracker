/// A [Store] on the local disk, using the same file names and formats as the Python app:
/// `kalshi_balance<suffix>.json` and `kalshi_trades<suffix>.csv`.
library;

import 'dart:convert';
import 'dart:io';

import 'store.dart';

class FileStore implements Store {
  final File _json, _csv;

  FileStore(String folder, {String suffix = ''})
      : _json = File('$folder/kalshi_balance$suffix.json'),
        _csv = File('$folder/kalshi_trades$suffix.csv');

  @override
  Map<String, dynamic>? loadState() {
    try {
      return jsonDecode(_json.readAsStringSync()) as Map<String, dynamic>;
    } catch (_) {
      return null; // no file yet, or not one we can read: start fresh
    }
  }

  @override
  void saveState(Map<String, dynamic> state) {
    try {
      final tmp = File('${_json.path}.tmp');
      tmp.writeAsStringSync(const JsonEncoder.withIndent('  ').convert(state));
      tmp.renameSync(_json.path); // swap in one step so it's never half-written
    } on FileSystemException {
      // a save that fails shouldn't stop the simulation
    }
  }

  @override
  void appendTrade(List<String> header, List<String> row) {
    try {
      final isNew = !_csv.existsSync();
      final lines = [if (isNew) header, row].map((r) => '${csvLine(r)}\r\n').join();
      _csv.writeAsStringSync(lines, mode: FileMode.append);
    } on FileSystemException {
      // a locked file (say, open in a spreadsheet) shouldn't stop the simulation
    }
  }

  @override
  void archive(String stamp) {
    for (final file in [_json, _csv]) {
      if (!file.existsSync()) continue;
      final dot = file.path.lastIndexOf('.');
      try {
        file.renameSync('${file.path.substring(0, dot)}_old_$stamp${file.path.substring(dot)}');
      } on FileSystemException {
        // leave it where it is
      }
    }
  }
}
