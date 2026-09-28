<script>
  import {
    pageUrl, candidates, variants, busyProbe, busyDeep, watching, watchTitle,
    doProbe, doDeepProbe, startWatch, stopWatch,
    loadVariants, download, downloadAll, copyUrl, previewUrl
  } from '../lib/store.js'
  import { kindLabel, kindClass, shortUrl } from '../lib/store.js'

  $: list = $candidates
  $: vmap = $variants
  $: canProbe = !$busyProbe && !$busyDeep && !$watching
</script>

<div class="urlbar card">
  <input
    bind:value={$pageUrl}
    placeholder="粘贴页面 URL / .m3u8 / .mpd / .mp4 …"
    on:keydown={(e) => e.key === 'Enter' && doProbe()}
  />
  <button class="btn primary" disabled={!canProbe} on:click={doProbe}>
    {$busyProbe ? '嗅探中…' : '轻嗅探'}
  </button>
  <button class="btn" disabled={!canProbe} on:click={doDeepProbe}>
    {$busyDeep ? '渲染中…' : '重嗅探'}
  </button>
  {#if !$watching}
    <button class="btn accent" disabled={$busyProbe || $busyDeep} on:click={startWatch}>浏览嗅探</button>
  {:else}
    <button class="btn danger" on:click={stopWatch}>停止浏览</button>
  {/if}
</div>

{#if $watching}
  <div class="watchbanner">
    <span class="dot"></span>
    浏览嗅探中{$watchTitle ? '：' + $watchTitle.slice(0, 40) : ''} —— 在受控浏览器里逛站，发现自动出现在这里
  </div>
{/if}

{#if list.length}
  <div class="sechead">
    <h3>发现 {list.length} 个可下载资源</h3>
    <button class="btn small primary" on:click={downloadAll}>全部下载</button>
  </div>
  <div class="grid">
    {#each list as c (c.kind + c.url)}
      <div class="cand card">
        <div class="cand-top">
          <span class="pill {kindClass(c)}">{kindLabel(c)}</span>
          {#if c.cookie}<span class="pill cookie" title="已携带登录态 Cookie 回源下载">🍪 登录态</span>{/if}
          <span class="cand-label">{c.label}</span>
        </div>
        <div class="cand-url" title={c.url}>{shortUrl(c.url, 90)}</div>
        {#if c.kind !== 'blob'}
          <div class="cand-actions">
            <button class="btn small" on:click={() => previewUrl.set(c.url)}>预览</button>
            {#if (c.kind === 'm3u8-media' || c.kind === 'mpd') && !vmap[c.url]}
              <button class="btn small" on:click={() => loadVariants(c)}>选清晰度</button>
            {/if}
            <button class="btn small" on:click={() => copyUrl(c.url)}>复制链接</button>
            <button class="btn small primary" on:click={() => download(c)}>下载</button>
          </div>
          {#if vmap[c.url]?.loading}
            <div class="muted">解析清晰度档位中…</div>
          {:else if vmap[c.url]?.list?.length}
            <select bind:value={vmap[c.url].selected}>
              {#each vmap[c.url].list as v}
                <option value={v.url}>{v.label}</option>
              {/each}
            </select>
          {/if}
        {:else}
          <div class="muted">blob 链接只在原页面有效，请看同页的 HLS / 直链项</div>
        {/if}
      </div>
    {/each}
  </div>
{:else}
  <div class="empty card">
    <div class="empty-title">还没有发现视频</div>
    <div class="muted">粘贴链接点「轻嗅探」；JS 渲染的站用「重嗅探」；想边逛边抓用「浏览嗅探」。仅支持公开非 DRM 资源学习用。</div>
  </div>
{/if}
