import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Badge } from '@/ui/display'
import { Legend, Panel } from '@/ui/console'
import { Button, Field, Input, Switch } from '@/ui/controls'
import { Icon } from '@/ui/icons'
import { toast } from '@/lib/toast'
import { netease, qk, type NeteaseQR } from './api'

/** Account, QR login and download-cache controls shared by every NetEase AutoDJ. */
export function NeteaseAccount() {
  const qc = useQueryClient()
  const status = useQuery({ queryKey: ['netease', 'status'], queryFn: () => netease.status(), refetchInterval: 30_000 })
  const refetchStatus = status.refetch
  const [qr, setQr] = useState<NeteaseQR | null>(null)
  const [scan, setScan] = useState<'waiting' | 'scanned' | 'expired'>('waiting')
  const [scanNick, setScanNick] = useState('')
  const [starting, setStarting] = useState(false)
  const [checking, setChecking] = useState(false)
  const [cacheOn, setCacheOn] = useState(true)
  const [cacheDir, setCacheDir] = useState('')
  const cacheDirty = useRef(false)

  useEffect(() => {
    const d = status.data
    if (!d || cacheDirty.current) return
    setCacheOn(d.cache_enabled)
    setCacheDir(d.cache_dir)
  }, [status.data])

  // Poll the current QR key until NetEase reports success or expiry.
  useEffect(() => {
    if (!qr) return
    let cancelled = false
    const timer = setInterval(async () => {
      try {
        const r = await netease.pollQR(qr.key)
        if (cancelled) return
        if (r.status === 'waiting') { setScan('waiting'); return }
        if (r.status === 'scanned') { setScan('scanned'); setScanNick((n) => r.nickname || n); return }
        if (r.status === 'success') {
          setScanNick((n) => r.nickname || n)
          setQr(null)
          toast.success(`NetEase login: ${r.nickname || 'signed in'}`)
          void refetchStatus()
          qc.invalidateQueries({ queryKey: qk.instances })
          return
        }
        setScan('expired')
        setQr(null)
        toast.error('The login QR expired. Start again.')
      } catch (e) {
        if (!cancelled) toast.error((e as Error).message || 'Could not poll the login QR')
      }
    }, 1500)
    return () => { cancelled = true; clearInterval(timer) }
  }, [qr, qc, refetchStatus])

  const startQR = async () => {
    setStarting(true)
    setScanNick('')
    try {
      setQr(await netease.startQR())
      setScan('waiting')
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the QR login')
    } finally { setStarting(false) }
  }

  const check = async () => {
    setChecking(true)
    try {
      const d = await netease.status(true)
      qc.setQueryData(['netease', 'status'], d)
    } catch (e) { toast.error((e as Error).message || 'Could not check the account') }
    finally { setChecking(false) }
  }

  const logout = async () => {
    try {
      await netease.logout()
      qc.invalidateQueries({ queryKey: qk.instances })
      await status.refetch()
      toast.success('NetEase account signed out')
    } catch (e) { toast.error((e as Error).message || 'Could not sign out') }
  }

  const saveCache = async () => {
    try {
      await netease.cache(cacheOn, cacheDir.trim())
      cacheDirty.current = false
      await status.refetch()
      toast.success(cacheOn ? 'NetEase downloads will be cached' : 'NetEase downloads will not be cached')
    } catch (e) { toast.error((e as Error).message || 'Could not save the cache settings') }
  }

  const d = status.data
  const state = !d ? 'loading' : d.logged_in ? 'signed in' : d.music_u_set ? 'cookie expired' : 'signed out'

  return (
    <Panel
      legend="NetEase Cloud Music"
      title="Account & cache"
      lamp={d?.logged_in ? 'live' : 'off'}
      actions={<>
        <Button size="sm" onClick={check} loading={checking}><Icon name="refresh" size={14} />Check</Button>
        {d?.logged_in && <Button size="sm" variant="danger" onClick={logout}>Sign out</Button>}
      </>}
    >
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone={d?.logged_in ? 'live' : d?.music_u_set ? 'warn' : 'neutral'}>{state}</Badge>
        {d?.nickname && <span className="text-sm text-ink">{d.nickname}</span>}
        {!d?.logged_in && !qr && <Button size="sm" variant="primary" onClick={startQR} loading={starting}><Icon name="key" size={14} />Scan QR to sign in</Button>}
      </div>

      {d?.last_error && <p className="mt-2 text-xs text-warn">Last check: {d.last_error}</p>}
      {!d?.logged_in && !qr && (
        <p className="mt-2 text-xs text-muted">
          The QR login is for the account that owns the playlists. The session stays in this config and is re-checked every ten minutes; playback asks for <span className="font-mono">exhigh</span> first, then <span className="font-mono">hires</span>, and accepts a lower level when the account cannot provide those.
        </p>
      )}

      {qr && (
        <div className="mt-4 flex flex-wrap items-start gap-5">
          <img src={qr.qr} alt="NetEase login QR code" className="h-48 w-48 rounded-lg bg-white p-2" />
          <div className="min-w-0 text-sm">
            <p className="text-ink">Scan with the NetEase Cloud Music app.</p>
            <p className="mt-1 text-xs text-muted">
              {scan === 'scanned' ? `Scanned${scanNick ? ` by ${scanNick}` : ''}; confirm on the phone.` : 'Waiting for a scan…'}
            </p>
            <p className="mt-3 text-xs text-faint">The code expires after about three minutes.</p>
          </div>
        </div>
      )}

      <div className="mt-5 border-t border-line pt-4">
        <Legend>Download cache</Legend>
        <div className="mt-3 space-y-3">
          <Switch label="Cache NetEase downloads" checked={cacheOn} onCheckedChange={(v) => { cacheDirty.current = true; setCacheOn(v) }} />
          <p className="text-xs text-muted">
            Files are keyed by music id, so a song that appears in several playlists downloads once. When off, each track is written to a temporary file and deleted after playback.
          </p>
          <div className="flex flex-wrap items-end gap-2">
            <Field label="Cache directory" hint="Relative paths are resolved against the server's working directory.">
              <Input mono value={cacheDir} onChange={(e) => { cacheDirty.current = true; setCacheDir(e.target.value) }} placeholder="netease-cache" />
            </Field>
            <Button onClick={saveCache} disabled={!cacheDirty.current}>Save</Button>
          </div>
        </div>
      </div>
    </Panel>
  )
}
