import { useEffect, useState } from 'react'
import { Link } from 'wouter'
import { useQueryClient } from '@tanstack/react-query'
import { Badge } from '@/ui/display'
import { Legend, Panel } from '@/ui/console'
import { Button, Switch } from '@/ui/controls'
import { Menu, useAction } from '@/ui/kit'
import { Icon } from '@/ui/icons'
import { toast } from '@/lib/toast'
import { dj, netease, qk, type DjInstance, type NeteaseCacheJob } from './api'
import { useDeck, useTransport } from './deckState'
import { NowPlaying, Progress, StateBadge, TransportRow } from './DeckParts'

/** One AutoDJ on the overview: live state, transport and what plays next. The full workspace is the Studio. */
export function InstanceCard({ inst, canAdmin, onEdit, onDelete }: {
  inst: DjInstance; canAdmin: boolean; onEdit: () => void; onDelete: () => void
}) {
  const qc = useQueryClient()
  const deck = useDeck(inst)!
  const { send, isBusy } = useTransport(inst.mount)
  const { run, isBusy: busy } = useAction()
  const toggle = () => run('toggle', async () => {
    await dj.toggle(inst.mount)
    qc.invalidateQueries({ queryKey: qk.instances })
  }, inst.enabled ? `${inst.mount} stopped` : `${inst.mount} started`)
  const studio = `/admin/studio?mount=${encodeURIComponent(inst.mount)}`
  const next = deck.queue.slice(0, 3)
  const [cacheJob, setCacheJob] = useState<NeteaseCacheJob | null>(null)

  // Reattach to a job that was started before this card mounted.
  useEffect(() => {
    if (!canAdmin || inst.source !== 'netease') return
    let cancelled = false
    netease.cachePlaylistStatus(inst.mount)
      .then((j) => { if (!cancelled && (j.active || j.total > 0)) setCacheJob(j) })
      .catch(() => { /* status is advisory; the button still works */ })
    return () => { cancelled = true }
  }, [canAdmin, inst.source, inst.mount])

  // Poll only while a batch is running. The server keeps the job alive
  // across tab switches, so progress is the same on any admin page.
  useEffect(() => {
    if (!cacheJob?.active) return
    let cancelled = false
    const timer = setInterval(async () => {
      try {
        const j = await netease.cachePlaylistStatus(inst.mount)
        if (cancelled) return
        setCacheJob(j)
        if (!j.active) {
          toast.success(`Cached ${j.cached ?? j.done}/${j.total} NetEase tracks${j.failed ? `, ${j.failed} failed` : ''}`)
        }
      } catch (e) {
        if (!cancelled) toast.error((e as Error).message || 'Could not read the cache progress')
      }
    }, 1500)
    return () => { cancelled = true; clearInterval(timer) }
  }, [cacheJob?.active, inst.mount])

  const cachePlaylist = () => run('cache-playlist', async () => {
    const j = await netease.cachePlaylist(inst.mount)
    setCacheJob(j)
    if (!j.active) toast.success(`All ${j.total} NetEase tracks are cached`)
    else toast.success(`Caching ${j.total} NetEase tracks`)
  }, undefined)

  return (
    <Panel
      legend="AutoDJ" title={inst.mount} lamp={!inst.enabled ? 'off' : deck.state === 'playing' ? 'live' : deck.state === 'paused' ? 'warn' : 'off'}
      actions={<>
        {canAdmin && <Switch label={`${inst.enabled ? 'Disable' : 'Enable'} ${inst.mount}`} checked={inst.enabled} onCheckedChange={toggle} disabled={busy('toggle')} />}
        {canAdmin && <Menu label={`Actions for ${inst.mount}`} actions={[
          { label: 'Edit settings', icon: 'edit', onSelect: onEdit },
          'sep',
          { label: 'Delete', icon: 'trash', danger: true, onSelect: onDelete },
        ]} />}
      </>}
    >
      <div className="flex flex-wrap items-center gap-1.5">
        <StateBadge state={inst.enabled ? deck.state : 'stopped'} />
        {!inst.enabled && <Badge tone="warn">Disabled</Badge>}
        <Badge>{(inst.format || 'mp3').toUpperCase()} {inst.bitrate} kbps</Badge>
        {inst.source === 'netease' && <Badge tone="info">NetEase · {inst.netease_playlists?.length || 0} playlist{(inst.netease_playlists?.length || 0) === 1 ? '' : 's'}</Badge>}
        {inst.idle_skip && <Badge>Idle skip</Badge>}
        {inst.auto_cache_playlist && <Badge>Auto-cache</Badge>}
        {!inst.visible && <span title="Not listed publicly; still streamable by URL"><Badge>Unlisted</Badge></span>}
        {inst.song_command && <Badge tone="info">Song command</Badge>}
        {inst.mpd_enabled && inst.mpd_port && <Badge tone="info">MPD {inst.mpd_port}</Badge>}
        <span className="ml-auto text-xs text-muted"><span className="num font-mono text-ink">{deck.playlistLen}</span> tracks</span>
      </div>

      <div className="mt-4"><NowPlaying deck={deck} /></div>
      <div className="mt-4"><Progress deck={deck} /></div>

      {!inst.enabled && <p className="mt-3 text-xs text-muted">Disabled. Turn the switch on to start playout; the playlist is kept.</p>}
      {inst.enabled && deck.state === 'stopped' && deck.playlistLen === 0 && !inst.song_command && (
        <p className="mt-3 text-xs text-muted">
          {inst.source === 'netease'
            ? 'No tracks. Check that the account is signed in and the playlist ids are readable, then wait for the next refresh.'
            : <>No tracks. {inst.music_dir ? <>The music directory <span className="font-mono">{inst.music_dir}</span> had nothing playable, or the playlist was cleared. </> : null}Add tracks in the Studio.</>}
        </p>
      )}

      <div className="mt-4 flex flex-wrap items-center justify-between gap-3">
        <TransportRow deck={deck} send={send} isBusy={isBusy} disabled={!inst.enabled} />
        <div className="flex items-center gap-2">
          {canAdmin && inst.source === 'netease' && (
            <Button
              size="sm"
              onClick={cachePlaylist}
              loading={busy('cache-playlist')}
              disabled={!inst.enabled || !!cacheJob?.active}
              title={!inst.enabled
                ? 'Start the AutoDJ once so its playlist can be loaded'
                : cacheJob?.error || 'Download every NetEase track in this playlist into the local cache'}
            >
              <Icon name="download" size={14} />
              {cacheJob?.active
                ? `Caching ${cacheJob.done + cacheJob.failed}/${cacheJob.total}`
                : cacheJob && cacheJob.total > 0
                  ? `Cache playlist (${cacheJob.cached ?? cacheJob.done}/${cacheJob.total})`
                  : 'Cache playlist'}
            </Button>
          )}
          <Link href={studio} className="inline-flex h-8 items-center gap-1.5 rounded-md border border-line-strong bg-raised px-3 text-[13px] font-medium text-ink hover:bg-hover">
            <Icon name="studio" size={14} />Studio
          </Link>
        </div>
      </div>

      {next.length > 0 && (
        <div className="mt-4 flex items-center gap-3 border-t border-line pt-3">
          <Legend className="shrink-0">Up next</Legend>
          <ol className="flex min-w-0 flex-1 items-center gap-2 overflow-hidden text-xs text-muted">
            {next.map((t, i) => (
              <li key={i} className="flex min-w-0 items-center gap-2">
                {i > 0 && <Icon name="chevron-right" size={12} className="shrink-0 text-faint" />}
                <span className="truncate" title={t.title}>{t.title || t.path}</span>
              </li>
            ))}
          </ol>
          {deck.queue.length > next.length && <span className="shrink-0 font-mono text-[11px] text-faint">+{deck.queue.length - next.length}</span>}
        </div>
      )}
    </Panel>
  )
}
