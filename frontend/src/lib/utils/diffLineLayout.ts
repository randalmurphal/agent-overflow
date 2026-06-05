export function diffScrollContentClass(wordWrap: boolean): string {
  return wordWrap ? 'w-full min-w-full' : 'w-max min-w-full';
}

export function diffSplitGridColumnsClass(wordWrap: boolean): string {
  return wordWrap
    ? 'grid-cols-[minmax(0,1fr)_minmax(0,1fr)]'
    : 'grid-cols-[minmax(50%,max-content)_minmax(50%,max-content)]';
}

export function diffLineContentClass(wordWrap: boolean): string {
  return wordWrap ? 'min-w-0 whitespace-pre-wrap break-all' : 'min-w-max whitespace-pre';
}

export function diffFlexLineContentClass(wordWrap: boolean): string {
  return wordWrap
    ? 'flex-1 min-w-0 whitespace-pre-wrap break-all'
    : 'min-w-max shrink-0 whitespace-pre';
}
