export interface StreamLogLine {
  cursor: number;
  stream: string;
  text: string;
}

export function subscribeEventStream(
  url: string,
  onEvent: (line: StreamLogLine) => void,
  onGap: () => void,
  onError: (err: Event) => void,
): () => void {
  const source = new EventSource(url, { withCredentials: true });
  source.addEventListener('log', (ev) => {
    try {
      const data = JSON.parse((ev as MessageEvent<string>).data) as StreamLogLine;
      onEvent(data);
    } catch {
      // Ignore malformed chunk
    }
  });
  source.addEventListener('gap', () => onGap());
  source.addEventListener('error', (err) => onError(err));

  return () => {
    source.close();
  };
}
