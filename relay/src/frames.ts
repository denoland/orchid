/// Tunnel frame protocol. JSON for control, binary chunks tagged with the
/// stream id. Each browser-originated request gets a fresh stream id; orch
/// responds with head+chunks+end frames carrying the same id.
///
/// Frames are JSON lines, except body chunks which are sent as binary
/// WebSocket messages prefixed with a 4-byte big-endian stream id.

export type Frame =
  | { t: 'req'; id: number; method: string; path: string; headers: [string, string][]; hasBody: boolean }
  | { t: 'req-end'; id: number }                                  // body finished (sent after binary chunks)
  | { t: 'res-head'; id: number; status: number; headers: [string, string][]; streaming: boolean }
  | { t: 'res-end'; id: number }
  | { t: 'cancel'; id: number }
  | { t: 'hello'; userId: number; login: string }                 // agent → relay handshake
  | { t: 'pong' }
  // WebSocket multiplexing. ws-open: relay → agent (upgrade request).
  // ws-text: text frame either direction. Binary WS frames piggyback on the
  // existing stream-id-prefixed binary message format. ws-close: either
  // direction; carries optional code/reason.
  | { t: 'ws-open'; id: number; path: string; headers: [string, string][] }
  | { t: 'ws-text'; id: number; data: string }
  | { t: 'ws-close'; id: number; code?: number; reason?: string }

export const CONTROL_TEXT = 'json'

export function encodeBinary(streamId: number, chunk: Uint8Array): Uint8Array {
  const out = new Uint8Array(4 + chunk.byteLength)
  const dv = new DataView(out.buffer)
  dv.setUint32(0, streamId, false)
  out.set(chunk, 4)
  return out
}

export function decodeBinary(buf: ArrayBuffer): { streamId: number; chunk: Uint8Array } {
  const dv = new DataView(buf)
  return {
    streamId: dv.getUint32(0, false),
    chunk: new Uint8Array(buf, 4),
  }
}
