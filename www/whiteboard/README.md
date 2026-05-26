# @orchid/whiteboard

The collaborative whiteboard primitives that power the orchid dashboard,
extracted as a standalone library.

```tsx
import { ReactFlow } from '@xyflow/react'
import {
  NoteNode, LinkNode, TextNode, StrokeNode,
  PenLayer, CollabLayer, useCollabSocket,
  pointsToPath,
} from '@orchid/whiteboard'

const nodeTypes = { note: NoteNode, link: LinkNode, text: TextNode, stroke: StrokeNode }

function Canvas() {
  const { cursors, sendCursor, send } = useCollabSocket({
    url: '/api/canvas/ws',
    onMessage: (msg) => { /* sync your nodes/edges */ },
  })
  return (
    <>
      <CollabLayer cursors={cursors} sendCursor={sendCursor} />
      <ReactFlow nodeTypes={nodeTypes} ... />
      <PenLayer active={tool === 'pen'} onStroke={...} />
    </>
  )
}
```

## What's included

- **Node types**: sticky `NoteNode`, OG-link `LinkNode`, free `TextNode`,
  free-hand `StrokeNode`.
- **Pen overlay**: `<PenLayer>` captures pointer strokes anywhere on the
  surface and emits a list of points.
- **Collaboration**: `useCollabSocket` opens a WebSocket to your relay,
  exposes cursors + a broadcast `send` for app-level state ops.
- **Cursor rendering**: `<CollabLayer>` paints remote cursors with the
  same colour/name treatment used in production.

## What's _not_ included

- React Flow itself — bring your own. We re-use its node-type API.
- Persistence — `useCollabSocket` only handles wire transport; how you
  reduce remote ops onto your local store is your call.
- The orchid-specific `CardNode` / `PaneWindowNode` / attention model.

## Status

Pulled out of orchid in a single big move. API may still shift while we
sand the seams. Pin to a commit if you ship against it.
