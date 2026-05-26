export interface Stroke {
  id: string
  x: number
  y: number
  w: number
  h: number
  d: string
  width: number
}

export interface UserNode {
  type: 'note' | 'link' | 'text'
  id: string
  x: number
  y: number
  data: Record<string, unknown>
}

export type LinkVariant =
  | 'youtube'
  | 'github-code'
  | 'gist'
  | 'docs'
  | 'meet'
  | 'pr'
  | 'issue'
  | 'generic'
