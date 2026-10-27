// In-call collaboration (ROADMAP P1) against the real daemon with Chromium
// fake devices: host + 2 guests (+ late/locked-out/waiting guests). Verifies:
// chat round trip with unread badge, history for a late joiner, an XSS payload
// rendered as text with safe links; raise hand by keyboard shortcut visible to
// the host (tile + queue); reactions on the sender's tile; host mute (call
// sender disabled, ISO mic track still live, indicators everywhere); room lock
// refusing a new guest with a clear message; host remove (removed guest told,
// tile gone for others); recording consent banner for everyone in the call and
// the waiting room. Build first; STUDIO_BINARY points at the daemon.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp,rm,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {startTake,launchURL,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-collab-'));
const port=Number(process.env.STUDIO_TEST_PORT||17843),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel'],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];let browser;
const pages=new Map();
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const XSS='<img src=x onerror="window.__xss=1"><script>window.__xss=2</script> **bold** https://example.com/notes?a=1&b=2.';
const tileOf=(page,name)=>page.locator('#video-grid .tile[data-participant-id]').filter({has:page.locator('.tile-name',{hasText:new RegExp(`^${name}$`)})});

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async(name,viewport={width:1280,height:800})=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport});
  await context.addInitScript(()=>{const Original=window.RTCPeerConnection;window.__pcs=[];window.RTCPeerConnection=class extends Original{constructor(...args){super(...args);window.__pcs.push(this);}};});
  const page=await context.newPage();page.setDefaultTimeout(30000);
  page.on('console',m=>{if(m.type()==='error'&&!(m.text().includes('422')&&m.location().url.endsWith('/invite')))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  pages.set(name,page);return page;
 };
 const host=await open('Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 const request=async name=>{
  const guest=await open(name);
  await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
  await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
  await guest.locator('#name-input').fill(name);await guest.locator('#join-btn').click();
  return guest;
 };
 const admit=async name=>{
  const guest=await request(name);
  await host.waitForFunction(name=>document.querySelector('#host-admission-banner').style.display==='flex'&&document.querySelector('#admission-guest-name').textContent===name,name);
  await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
  return guest;
 };
 const a=await admit('Guest A');
 await tileOf(host,'Guest A').waitFor();await tileOf(a,'Host').waitFor();
 console.log('host + Guest A in call');

 // 1. Chat round trip with unread badge.
 await host.locator('#chat-btn').click();
 assert.equal(await host.evaluate(()=>document.activeElement?.id),'chat-input','chat opens with focus in the composer');
 await host.locator('#chat-input').fill('Welcome to the show');await host.keyboard.press('Enter');
 await a.locator('#chat-btn .collab-badge',{hasText:'1'}).waitFor();
 assert.match(await a.locator('#chat-btn').getAttribute('aria-label'),/1 unread/);
 await a.locator('#chat-btn').click();
 await a.locator('.chat-log .chat-text',{hasText:'Welcome to the show'}).waitFor();
 assert.equal(await a.locator('#chat-btn .collab-badge').isHidden(),true,'badge clears when chat opens');
 await a.locator('#chat-input').fill(XSS);await a.locator('.chat-send').click();
 await host.locator('.chat-log .chat-msg').nth(1).waitFor();
 console.log('chat round trip + unread badge');

 // 2. Late joiner sees history; XSS renders as text; links are safe.
 const b=await admit('Guest B');
 await b.locator('#chat-btn .collab-badge').isHidden();
 await b.locator('#chat-btn').click();
 await b.locator('.chat-log .chat-msg').nth(1).waitFor();
 for(const page of [host,b]){
  const texts=await page.locator('.chat-log .chat-text').allTextContents();
  assert.deepEqual(texts,['Welcome to the show',XSS]);
  assert.equal(await page.locator('.chat-log img, .chat-log script, .chat-log b').count(),0,'markup was interpreted');
  assert.equal(await page.evaluate(()=>window.__xss),undefined,'XSS executed');
  const link=page.locator('.chat-log a');
  assert.equal(await link.count(),1);
  assert.equal(await link.getAttribute('href'),'https://example.com/notes?a=1&b=2');
  assert.equal(await link.getAttribute('target'),'_blank');
  assert.equal(await link.getAttribute('rel'),'noopener noreferrer');
 }
 const names=await b.locator('.chat-meta strong').allTextContents();
 assert.deepEqual(names,['Host','Guest A']);
 for(const page of [host,a,b])await page.keyboard.press('Escape');
 console.log('late joiner got history; XSS payload is inert text');

 // 3. Raise hand with the Meet shortcut; host sees tile badge + queue.
 const isMac=await a.evaluate(()=>/Mac|iPhone|iPad/.test(navigator.platform||navigator.userAgent));
 await a.locator('body').click({position:{x:5,y:5}});
 await a.keyboard.press(isMac?'Control+Meta+KeyH':'Control+Alt+KeyH');
 await tileOf(host,'Guest A').locator('.tile-hand').waitFor();
 await tileOf(b,'Guest A').locator('.tile-hand').waitFor();
 await a.locator('#local-tile .tile-hand').waitFor();
 assert.equal(await a.locator('#hand-btn').getAttribute('aria-pressed'),'true');
 await host.locator('#people-btn').click();
 assert.deepEqual(await host.locator('.hand-queue-list li').allTextContents(),['Guest A']);
 // Typing "?" in chat must not open the shortcut help.
 await b.locator('#chat-btn').click();await b.locator('#chat-input').type('?');
 assert.equal(await b.locator('#shortcut-dialog').evaluate(d=>d.open),false);
 await b.keyboard.press('Escape');
 await b.locator('body').click({position:{x:5,y:5}});await b.keyboard.press('Shift+Slash');
 assert.equal(await b.locator('#shortcut-dialog').evaluate(d=>d.open),true,'? opens shortcut help');
 await b.keyboard.press('Escape');
 console.log('raise hand via shortcut visible to host; ? help respects typing');

 // 4. Reaction floats on the sender's tile everywhere.
 await b.locator('#react-btn').click();await b.locator('.reaction-choice[data-emoji="🎉"]').click();
 await tileOf(host,'Guest B').locator('.reaction-float',{hasText:'🎉'}).waitFor();
 await tileOf(a,'Guest B').locator('.reaction-float',{hasText:'🎉'}).waitFor();
 await b.locator('#local-tile .reaction-float',{hasText:'🎉'}).waitFor();
 console.log('reaction shown on the sender tile');

 // 5. Host mutes Guest A: call sender silenced, ISO mic track still live.
 await host.locator('.people-row',{has:host.locator('strong',{hasText:/^Guest A$/})}).locator('[data-action="mute"]').click();
 await a.waitForFunction(()=>{const pc=window.__pcs.filter(p=>p.connectionState!=='closed').at(-1);const s=pc?.getSenders().find(s=>s.track?.kind==='audio');return s&&s.track.enabled===false;});
 assert.equal(await a.evaluate(()=>document.querySelector('#local-video').srcObject.getAudioTracks()[0].enabled),true,'host mute must not silence the recording track');
 assert.match(await a.locator('#toggle-mic-btn').textContent(),/host muted you/);
 await a.locator('#local-tile .tile-state-mic').waitFor();
 for(const page of [host,b])await page.waitForFunction(()=>{const t=[...document.querySelectorAll('.tile[data-participant-id]')].find(t=>t.querySelector('.tile-name').textContent==='Guest A');return t&&!t.querySelector('.tile-state-mic').hidden;});
 await host.locator('.people-row',{has:host.locator('strong',{hasText:/^Guest A$/})}).locator('.chip-mic.on',{hasText:'Mic off'}).waitFor();
 // Unmute restores the call sender.
 await a.locator('#toggle-mic-btn').click();
 await a.waitForFunction(()=>{const pc=window.__pcs.filter(p=>p.connectionState!=='closed').at(-1);return pc.getSenders().find(s=>s.track?.kind==='audio').track.enabled===true;});
 await host.waitForFunction(()=>{const t=[...document.querySelectorAll('.tile[data-participant-id]')].find(t=>t.querySelector('.tile-name').textContent==='Guest A');return t.querySelector('.tile-state-mic').hidden;});
 console.log('host mute: call muted, recording track live, indicators updated');

 // 6. Lock: a new guest is refused with a clear message.
 await host.locator('.people-lock .switch').click();
 await host.waitForFunction(()=>document.querySelector('.people-lock .switch').getAttribute('aria-checked')==='true');
 const d=await request('Guest D');
 await d.locator('#removed-dialog[open]',{hasText:'This call is locked'}).waitFor();
 assert.match(await d.locator('#diagnostic-status').textContent(),/locked/i);
 // Behind the dialog the waiting room says so too, not "Waiting for admission".
 assert.equal(await d.locator('#waiting-room h1').textContent(),'This call is locked.');
 assert.equal(await d.locator('#waiting-room .badge').textContent(),'Locked');
 await wait(500);
 assert.notEqual(await host.evaluate(()=>document.querySelector('#host-admission-banner').style.display),'flex','locked room still asks the host');
 await screenshot('callfeatures-locked-guest-1280',d);
 await d.context().close();pages.delete('Guest D');
 console.log('locked room refuses new guest');

 // 7. Remove Guest B (confirm dialog); B is told; tile vanishes for others.
 await host.locator('.people-row',{has:host.locator('strong',{hasText:/^Guest B$/})}).locator('[data-action="remove"]').click();
 await host.locator('.confirm-dialog[open] [data-confirm="remove"]').click();
 await b.locator('#removed-dialog[open]',{hasText:'You were removed'}).waitFor();
 await tileOf(host,'Guest B').waitFor({state:'detached'});
 await tileOf(a,'Guest B').waitFor({state:'detached'});
 await host.waitForFunction(()=>document.querySelectorAll('.people-row').length===2);
 const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
 assert.equal(detail.participants.find(p=>p.name==='Guest B')?.status,'denied','removal did not revoke');
 await b.context().close();pages.delete('Guest B');
 await host.locator('.people-lock .switch').click();
 await host.waitForFunction(()=>document.querySelector('.people-lock .switch').getAttribute('aria-checked')==='false');
 console.log('remove revokes and removes the tile');

 // 8. Recording consent banner for everyone, and the waiting room.
 await startTake(host);
 for(const page of [host,a]){
  await page.locator('#recording-banner').waitFor({state:'visible'});
  await page.waitForFunction(()=>/Recording/.test(document.querySelector('#recording-announcer').textContent));
 }
 const c=await request('Guest C');
 await c.locator('#waiting-recording-notice',{hasText:'being recorded'}).waitFor();
 await host.locator('#chat-btn').click();
 await host.locator('#chat-input').fill('Rolling now. Raise a hand if you need a pause.');await host.keyboard.press('Enter');
 await a.locator('#hand-btn').click();await a.locator('#hand-btn').click();
 await host.waitForFunction(()=>document.querySelectorAll('.toast').length===0,null,{timeout:10000}).catch(()=>{});
 await host.evaluate(()=>window.scrollTo(0,0));
 await screenshot('callfeatures-01-host-desktop-chat',host);
 await host.locator('#people-btn').click();
 await host.setViewportSize({width:390,height:844});
 assert.ok(await host.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),'390px overflow');
 await screenshot('callfeatures-02-host-390-people',host);
 await host.keyboard.press('Escape');
 await screenshot('callfeatures-03-host-390-dock',host);
 await host.setViewportSize({width:1280,height:800});
 await screenshot('callfeatures-04-waiting-room-recording',c);
 // At 1280 px the dock (inputs, collaboration, record, actions) fits one row.
 const dockRows=await host.evaluate(()=>new Set([...document.querySelectorAll('.dock > *')].filter(e=>e.offsetParent).map(e=>Math.round(e.getBoundingClientRect().top+e.getBoundingClientRect().height/2))).size);
 await host.locator('.dock').scrollIntoViewIfNeeded();
 await screenshot('callfeatures-05-host-desktop-dock',host);
 assert.equal(dockRows,1,'dock wraps at 1280px');
 await wait(1500);
 await host.locator('#rec-btn').click();
 for(const page of [host,a])await page.locator('#recording-banner').waitFor({state:'hidden'});
 await c.waitForFunction(()=>document.querySelector('#waiting-recording-notice').hidden);
 console.log('recording banner + announcements + waiting-room notice');

 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({chat:true,unreadBadge:true,lateJoinerHistory:true,xssInert:true,handShortcut:true,reaction:true,hostMuteCallOnly:true,lockRefuses:true,removeRevokes:true,recordingBanner:true,waitingNotice:true,errors}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 for(const [name,page] of pages)if(!page.isClosed())console.error(`--- ${name}:`,(await page.locator('body').innerText({timeout:2000}).catch(()=>'')).slice(-1500));
 console.error(logs.slice(-6000));throw e;
}finally{
 await browser?.close();
 if(daemon.exitCode===null&&daemon.signalCode===null){
  daemon.kill('SIGTERM');
  await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);
  if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');
 }
 await rm(data,{recursive:true,force:true});
}
