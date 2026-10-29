const DEFAULT_VALID_TYPES = new Set([
  "pdf",
  "txt",
  "md",
  "docx",
  "doc",
  "pptx",
  "ppt",
  "epub",
  "xmind",
  "html",
  "htm",
  "mhtml",
  "jpg",
  "jpeg",
  "png",
  "csv",
  "xlsx",
  "xls",
  "mp3",
  "wav",
  "m4a",
  "flac",
  "ogg",
]);

export function shouldRejectKnowledgeFileType(
  filename: string,
  validTypes?: Set<string> | string[],
) {
  const provided = validTypes
    ? validTypes instanceof Set
      ? validTypes
      : new Set(validTypes)
    : undefined;
  const allowed = provided && provided.size > 0 ? provided : DEFAULT_VALID_TYPES;
  const type = filename.substring(filename.lastIndexOf(".") + 1).toLowerCase();

  // Zip knowledge packages (Markdown documents plus their images) are
  // expanded into one document each by the upload endpoint itself. They are
  // not a parser-engine type, so they bypass the engine-driven whitelist.
  if (type === "zip") return false;

  return !allowed.has(type);
}
