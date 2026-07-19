// Faithful JS port of backend/internal/providers/decomposition/sentences.go.
// Produces IDENTICAL sentence boundaries to the Go SegmentSentences so
// the frontend can match stored sentence_index values (from
// fact_references) to rendered spans. Keep in sync with the Go file
// when rules change — a drift breaks fact highlighting silently.
//
// Returns [{index, start, end, text}] where start/end are absolute
// rune offsets into the source text (end exclusive). "Rune" here means
// UTF-16 code unit for BMP characters and a surrogate pair counts as
// 2 — this matches the Go rune count for all BMP text. The backend
// uses Unicode runes (Go range over string); JS String.prototype
// length is UTF-16. For source markdown (overwhelmingly BMP) the two
// align. For astral-plane characters (emoji) they diverge by 1 per
// character. The backend stores the Go-rune offsets, so to match
// exactly we iterate with Array.from(text) (which splits on code
// points, matching Go runes) instead of str.length.

export function segmentSentences(text) {
  if (!text) return [];
  const runes = Array.from(text);
  const lines = splitLinesKeepEnds(runes);

  const units = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const kind = classifyLine(line, units, i);

    switch (kind) {
      case "fencedCode": {
        const start = i;
        const fence = fenceMarker(line);
        i++;
        while (i < lines.length && !isClosingFence(lines[i], fence)) i++;
        if (i < lines.length) i++;
        units.push({ startLine: start, endLine: i, kind: "code" });
        break;
      }
      case "indentedCode": {
        const start = i;
        while (i < lines.length && isIndentedCodeLine(lines[i])) i++;
        units.push({ startLine: start, endLine: i, kind: "code" });
        break;
      }
      case "atxHeading":
        units.push({ startLine: i, endLine: i + 1, kind: "heading" });
        i++;
        break;
      case "setextHeading": {
        const prev = units[units.length - 1];
        if (prev && prev.kind === "prose" && prev.endLine === i) {
          prev.kind = "heading";
          prev.endLine = i + 1;
          i++;
        } else {
          units.push({ startLine: i, endLine: i + 1, kind: "heading" });
          i++;
        }
        break;
      }
      case "table": {
        const start = i;
        i++;
        while (i < lines.length && isTableRow(lines[i])) i++;
        units.push({ startLine: start, endLine: i, kind: "table" });
        break;
      }
      case "listItem": {
        const start = i;
        while (i < lines.length && (isListItem(lines[i]) || isContinuationLine(lines[i]))) i++;
        units.push({ startLine: start, endLine: i, kind: "list" });
        break;
      }
      case "blank":
        i++;
        break;
      default: {
        const start = i;
        while (i < lines.length && classifyLine(lines[i], units, i) === "prose") i++;
        units.push({ startLine: start, endLine: i, kind: "prose" });
      }
    }
  }

  const chunks = [];
  for (const u of units) {
    const startRune = lineStartRune(lines, u.startLine);
    const endRune = lineEndRune(lines, u.endLine, runes.length);
    const unitText = runes.slice(startRune, endRune).join("");
    if (u.kind === "prose") {
      const sub = splitProseSentences(Array.from(unitText), startRune);
      for (const s of sub) {
        s.index = chunks.length;
        chunks.push(s);
      }
      continue;
    }
    chunks.push({ index: chunks.length, text: unitText, start: startRune, end: endRune });
  }
  return chunks;
}

function splitLinesKeepEnds(runes) {
  const lines = [];
  let start = 0;
  for (let i = 0; i < runes.length; i++) {
    if (runes[i] === "\n") {
      lines.push(runes.slice(start, i + 1));
      start = i + 1;
    }
  }
  if (start < runes.length) lines.push(runes.slice(start));
  return lines;
}

function classifyLine(line, prev, idx) {
  const trimmed = trimLeftSpaces(line);
  if (trimmed.length === 0 || (trimmed.length === 1 && trimmed[0] === "\n")) return "blank";
  if (isFenceOpen(trimmed)) return "fencedCode";
  if (isIndentedCodeLine(line)) return "indentedCode";
  if (isATXHeading(trimmed)) return "atxHeading";
  if (isSetextUnderline(trimmed) && prev.length > 0) {
    const p = prev[prev.length - 1];
    if (p.kind === "prose" && p.endLine === idx) return "setextHeading";
  }
  if (isTableRow(line)) return "table";
  if (isListItem(line)) return "listItem";
  return "prose";
}

function trimLeftSpaces(runes) {
  let i = 0;
  while (i < runes.length && (runes[i] === " " || runes[i] === "\t")) i++;
  return runes.slice(i);
}

function isFenceOpen(trimmed) {
  if (trimmed.length < 3) return false;
  const marker = trimmed[0];
  if (marker !== "`" && marker !== "~") return false;
  let count = 0;
  while (count < trimmed.length && trimmed[count] === marker) count++;
  return count >= 3;
}

function fenceMarker(line) {
  const t = trimLeftSpaces(line);
  return t.length ? t[0] : "";
}

function isClosingFence(line, marker) {
  const t = trimLeftSpaces(line);
  let count = 0;
  while (count < t.length && t[count] === marker) count++;
  return count >= 3;
}

function isIndentedCodeLine(line) {
  if (line.length >= 1 && line[0] === "\t") return true;
  if (line.length >= 4 && line[0] === " " && line[1] === " " && line[2] === " " && line[3] === " ")
    return true;
  return false;
}

function isATXHeading(trimmed) {
  if (!trimmed.length || trimmed[0] !== "#") return false;
  let n = 0;
  while (n < trimmed.length && trimmed[n] === "#") n++;
  if (n > 6) return false;
  if (n < trimmed.length && trimmed[n] !== " " && trimmed[n] !== "\n") return false;
  return true;
}

function isSetextUnderline(trimmed) {
  if (trimmed.length < 3) return false;
  const marker = trimmed[0];
  if (marker !== "=" && marker !== "-") return false;
  for (let i = 0; i < trimmed.length; i++) {
    const r = trimmed[i];
    if (r !== marker && r !== " " && r !== "\t" && r !== "\n") return false;
  }
  return true;
}

function isTableRow(line) {
  const t = trimLeftSpaces(line);
  if (!t.length) return false;
  if (t[0] === "|") return true;
  for (let i = 0; i < t.length; i++) {
    if (t[i] === "|") return true;
    if (t[i] === "\n") break;
  }
  return false;
}

function isListItem(line) {
  const t = trimLeftSpaces(line);
  if (!t.length) return false;
  if ((t[0] === "-" || t[0] === "*" || t[0] === "+") && t.length > 1 && t[1] === " ") return true;
  let i = 0;
  while (i < t.length && t[i] >= "0" && t[i] <= "9") i++;
  if (i === 0) return false;
  if (i < t.length && (t[i] === "." || t[i] === ")") && i + 1 < t.length && t[i + 1] === " ")
    return true;
  return false;
}

function isContinuationLine(line) {
  if (line.length === 0) return true;
  if (line[0] === "\t") return true;
  if (line.length >= 2 && line[0] === " " && line[1] === " ") return true;
  for (const r of line) {
    if (r !== " " && r !== "\t" && r !== "\n") return false;
  }
  return true;
}

function lineStartRune(lines, lineIdx) {
  if (lineIdx >= lines.length) lineIdx = lines.length - 1;
  let off = 0;
  for (let i = 0; i < lineIdx; i++) off += lines[i].length;
  return off;
}

function lineEndRune(lines, endLine, total) {
  if (endLine >= lines.length) return total;
  let off = 0;
  for (let i = 0; i < endLine; i++) off += lines[i].length;
  return off;
}

function splitProseSentences(unitRunes, baseRune) {
  if (!unitRunes.length) return [];
  const chunks = [];
  let sentStart = 0;
  for (let i = 0; i < unitRunes.length; i++) {
    const r = unitRunes[i];
    if (r !== "." && r !== "!" && r !== "?") continue;
    let next = "";
    if (i + 1 < unitRunes.length) next = unitRunes[i + 1];
    if (next === " " || next === "\t" || next === "\n" || next === "") {
      let end = i + 1;
      while (
        end < unitRunes.length &&
        (unitRunes[end] === " " || unitRunes[end] === "\t" || unitRunes[end] === "\n")
      )
        end++;
      const text = unitRunes.slice(sentStart, end).join("");
      if (trimRunes(text) !== "") {
        chunks.push({ text, start: baseRune + sentStart, end: baseRune + end });
      }
      sentStart = end;
    }
  }
  if (sentStart < unitRunes.length) {
    const text = unitRunes.slice(sentStart).join("");
    if (trimRunes(text) !== "") {
      chunks.push({ text, start: baseRune + sentStart, end: baseRune + unitRunes.length });
    }
  }
  return chunks;
}

function trimRunes(s) {
  const runes = Array.from(s);
  let i = 0;
  while (i < runes.length && (runes[i] === " " || runes[i] === "\t" || runes[i] === "\n")) i++;
  let j = runes.length;
  while (j > i && (runes[j - 1] === " " || runes[j - 1] === "\t" || runes[j - 1] === "\n")) j--;
  return runes.slice(i, j).join("");
}
