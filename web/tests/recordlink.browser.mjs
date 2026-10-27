// Async record links end to end [SL-P5-3] with Chromium fake devices.
// The host creates a record link with a script on the home screen. A guest
// opens it (no host in any call), records a 20 s take, deletes it and
// re-records, then records a second 20 s take with the uncompressed mic,
// sends it while its upload is still blocked, and the tab is killed. The
// link is reopened in the same browser profile: the upload resumes and
// finishes. The host sees the submission, exports it (editor ZIP probed with
// ffprobe) and downloads the originals ZIP. An expired link and a rotated
// link are refused.
//   make build && cd web && STUDIO_TEST_PORT=19371 STUDIO_BINARY=$PWD/../bin/p2pvideod node tests/recordlink.browser.mjs
// Optional: STUDIO_SCREENSHOT_DIR=<dir> for p53-* screenshots, TAKE_SEC (default 20).
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,mkdir,writeFile,readdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {adminHeaders,freePort,isolatedNetworkArgs,launchURL} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const TAKE_MS=Number(process.env.TAKE_SEC||20)*1000;
const data=await mkdtemp(path.join(tmpdir(),'studio-recordlink-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const shots=process.env.STUDIO_SCREENSHOT_DIR;
if(shots)await mkdir(shots,{recursive:true});
const shot=async(name,page,opts={})=>{if(shots)await page.screenshot({path:path.join(shots,`p53-${name}.png`),animations:'disabled',...opts});};
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='',browser;const errors=[];daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const t0=Date.now(),ts=()=>((Date.now()-t0)/1000).toFixed(1);
const log=m=>console.log(`[${ts()}s] ${m}`);
try{
 let ready=false;for(let i=0;i<100&&!ready;i++){try{ready=(await fetch(origin+'/health')).ok}catch{}if(!ready)await wait(100);}assert.ok(ready,'daemon did not start');
 const admin=await adminHeaders(data);
 const links=async()=>(await(await fetch(`${origin}/api/v1/recordlinks`,{headers:admin})).json()).links;
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const watch=(page,name)=>{page.setDefaultTimeout(45000);page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));page.on('console',m=>{if(m.type()==='error'&&!/Failed to load resource|ERR_CONNECTION|ERR_FAILED/.test(m.text()))errors.push(`${name} console: ${m.text()}`);});};

 // 1. Host creates a record link with a script on the home screen.
 const hostCtx=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:900}});
 const host=await hostCtx.newPage();watch(host,'host');
 await host.goto(await launchURL(data,origin));
 await host.locator('#rail-recordings').click();
 await host.locator('#recordings-panel').waitFor();
 await host.locator('#record-link-new').click();
 await host.locator('#rl-title').fill('Intro for episode 12');
 await host.locator('#rl-prompt').fill('# Intro\nSay your name and what you are working on.\n\n## Then\nTell us one thing you learned this year.');
 await host.locator('#rl-limit').selectOption('300');
 await host.locator('#rl-create').click();
 await host.locator('.record-link').first().waitFor();
 let [link]=await links();
 assert.equal(link.title,'Intro for episode 12');
 assert.equal(link.maxDurationSec,300);
 assert.ok(link.url&&new URL(link.url).pathname==='/record',link.url);
 const guestURL=link.url;
 assert.ok(!(await host.locator('#recordings-panel').innerText()).includes(new URL(guestURL).searchParams.get('token')),'link secret rendered in the dashboard');
 log('host created record link');

 // 2. Guest opens it: green room, script, devices, pre-flight.
 const guestCtx=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:860}});
 const open=async()=>{const p=await guestCtx.newPage();watch(p,'guest');await p.goto(guestURL);return p;};
 let guest=await open();
 await guest.waitForFunction(()=>document.querySelector('#rec-preview').videoWidth>0);
 assert.equal(new URL(guest.url()).searchParams.get('token'),null,'secret left in the address bar');
 assert.match(await guest.locator('#rec-script').innerText(),/Say your name/);
 assert.equal(await guest.locator('#rec-title').innerText(),'Intro for episode 12');
 await guest.locator('#rec-name').fill('Grace Hopper');
 await guest.waitForFunction(()=>!document.querySelector('#rec-record').disabled);
 await guest.waitForFunction(()=>document.querySelectorAll('#rec-preflight .rec-check').length>=5);
 await shot('guest-setup-1280',guest);
 const phase=(p,want,timeout=45000)=>p.waitForFunction(w=>document.body.dataset.phase===w,want,{timeout});
 const recordTake=async(p,ms)=>{
  await p.locator('#rec-record').click();
  await phase(p,'recording',20000);
  await wait(ms);
  await p.locator('#rec-record').click();
  await phase(p,'review',30000);
 };

 // 3. Take 1, then re-record: delete it.
 await guest.locator('#rec-scroll').check();
 await guest.locator('#rec-record').click();
 await phase(guest,'recording',20000);
 await wait(3000);
 await shot('guest-recording-1280',guest);
 await wait(TAKE_MS-3000);
 await guest.locator('#rec-record').click();
 await phase(guest,'review',30000);
 await guest.waitForFunction(()=>!document.querySelector('#rec-review').hidden&&document.querySelector('#rec-review').readyState>=1,{},{timeout:15000});
 const reviewSec=await guest.evaluate(()=>document.querySelector('#rec-review').duration);
 log(`take 1 recorded; local review plays (${Number.isFinite(reviewSec)?reviewSec.toFixed(1)+' s':'streaming'})`);
 await guest.waitForFunction(()=>document.querySelector('.rec-take')?.dataset.uploaded==='true',{},{timeout:60000});
 await shot('guest-review-1280',guest);
 const draft=await guest.evaluate(()=>Object.entries(localStorage).find(([k])=>k.startsWith('studiolink.record.'))?.[1]);
 const {submissionId}=JSON.parse(draft);
 const take1=await guest.locator('.rec-take').first().getAttribute('data-take-id');
 await guest.locator('.rec-take [data-act=delete]').click();
 await guest.locator('#rec-dialog-ok').click();
 await guest.waitForFunction(()=>document.querySelectorAll('.rec-take').length===0);
 assert.equal(await guest.evaluate(()=>document.body.dataset.phase),'setup');
 log('take 1 deleted (re-record)');

 // 4. Take 2 with the uncompressed mic; its upload is cut halfway.
 await guest.locator('#rec-pcm').check();
 await guest.locator('#rec-record').click();
 await phase(guest,'recording',20000);
 await wait(TAKE_MS/2);
 let blocked=0;await guestCtx.route('**/api/v1/takes/*/tracks/*/chunks/*',route=>{blocked++;return route.abort('connectionfailed');});
 await wait(TAKE_MS/2);
 await guest.locator('#rec-record').click();
 await phase(guest,'review',30000);
 const take2=await guest.locator('.rec-take').first().getAttribute('data-take-id');
 assert.notEqual(take2,take1);
 await guest.locator('#rec-submit').click();
 await guest.locator('#rec-dialog-ok').click();
 await phase(guest,'submitted',20000);
 assert.equal(await guest.locator('.rec-take').first().getAttribute('data-uploaded'),'false','upload finished before the tab kill');
 assert.match(await guest.locator('#rec-done-title').innerText(),/Finishing the upload/);
 const subBefore=(await links())[0].submissions.find(s=>s.id===submissionId);
 assert.equal(subBefore.status,'submitted');
 assert.equal(subBefore.takes.length,1);
 assert.equal(subBefore.takes[0].originalsReady,false);
 await shot('guest-submitted-uploading-1280',guest);
 log(`take 2 sent with its upload blocked (${blocked} chunk requests dropped); killing tab`);
 await guest.close({runBeforeUnload:false});
 await guestCtx.unroute('**/api/v1/takes/*/tracks/*/chunks/*');

 // 5. Reopen the link in the same browser: uploads resume and finish.
 guest=await open();
 await phase(guest,'submitted',20000);
 await guest.waitForFunction(()=>document.querySelector('#rec-done-title')?.textContent==='Sent to the host',{},{timeout:90000});
 assert.equal(await guest.locator('.rec-take').first().getAttribute('data-uploaded'),'true');
 await shot('guest-submitted-1280',guest);
 await guest.setViewportSize({width:390,height:844});
 await shot('guest-submitted-390',guest,{fullPage:true});
 log('reopened link resumed the upload');
 [link]=await links();
 const sub=link.submissions.find(s=>s.id===submissionId);
 assert.equal(link.submitted,1);
 assert.equal(sub.name,'Grace Hopper');
 assert.equal(sub.takes.length,1,'the deleted take is gone on the host');
 assert.equal(sub.takes[0].id,take2);
 assert.equal(sub.takes[0].tracks,2,'camera + uncompressed mic');
 assert.ok(sub.takes[0].originalsReady,JSON.stringify(sub.takes[0]));
 assert.ok(sub.takes[0].durationSec>=TAKE_MS/1000-1&&sub.takes[0].durationSec<TAKE_MS/1000+4,`take duration ${sub.takes[0].durationSec}`);
 const auth={headers:{Authorization:`Bearer ${sub.hostToken}`}};
 const detail=await(await fetch(`${origin}/api/v1/takes/${take2}`,auth)).json();
 assert.deepEqual(detail.tracks.map(t=>t.status).sort(),['assembled','assembled'],JSON.stringify(detail.tracks));
 assert.ok(detail.tracks.some(t=>t.kind==='audio'&&t.sourceLabel==='Uncompressed microphone'));

 // 6. Host: the submission in Recordings; export it and download both ZIPs.
 await host.reload();
 await host.locator(`.record-take[data-take-id="${take2}"]`).waitFor();
 await host.locator(`.record-take[data-take-id="${take2}"] [data-act=export]`).click();
 await host.locator(`.record-take[data-take-id="${take2}"] [data-file=editor]`).waitFor({timeout:180000});
 await host.locator('#recordings-panel').scrollIntoViewIfNeeded();
 await shot('host-recordings-1280',host);
 await shot('host-recordings-panel',host.locator('#recordings-panel'));
 await host.locator(`.record-take[data-take-id="${take2}"] [data-act=handoffs]`).click();
 await host.locator('.record-handoff-dialog .ecosystem-item').first().waitFor({timeout:20000});
 await host.locator('.record-handoff-dialog button[value=ok]').click();
 log('host exported the submission');
 const unzip=async(url,name)=>{
  const res=await fetch(url,auth);assert.equal(res.status,200,`${name}: ${res.status}`);
  const file=path.join(data,`${name}.zip`);await writeFile(file,Buffer.from(await res.arrayBuffer()));
  const dir=path.join(data,name);await mkdir(dir,{recursive:true});
  execFileSync('unzip',['-q','-o',file,'-d',dir]);
  const all=[];const walk=async d=>{for(const e of await readdir(d,{withFileTypes:true})){const p=path.join(d,e.name);if(e.isDirectory())await walk(p);else all.push(p);}};await walk(dir);
  return all;
 };
 const editor=await unzip(`${origin}/api/v1/takes/${take2}/download`,'editor');
 assert.ok(editor.some(f=>f.endsWith('manifest.json')),'editor ZIP lacks manifest.json');
 const media=editor.filter(f=>/\.(mov|mp4|wav)$/i.test(f));
 assert.ok(media.some(f=>/\.(mov|mp4)$/i.test(f))&&media.some(f=>/\.wav$/i.test(f)),editor.join('\n'));
 for(const f of media){
  const out=execFileSync('ffprobe',['-v','error','-show_entries','format=duration:stream=codec_type','-of','json',f]).toString();
  const probe=JSON.parse(out);
  assert.ok(Number(probe.format.duration)>TAKE_MS/1000-2,`${path.basename(f)} is ${probe.format.duration}s`);
  execFileSync('ffmpeg',['-v','error','-i',f,'-f','null','-']);
 }
 const originals=await unzip(`${origin}/api/v1/takes/${take2}/originals`,'originals');
 assert.ok(originals.some(f=>/\.wav$/i.test(f))&&originals.some(f=>/\.(webm|mp4)$/i.test(f)),originals.join('\n'));
 log(`editor ZIP decodes (${media.length} media files); originals ZIP has ${originals.length} files`);

 // 7. A rotated link and an expired link are refused.
 const oldURL=guestURL;
 await fetch(`${origin}/api/v1/recordlinks/${link.id}/rotate`,{method:'POST',headers:admin});
 const stale=await guestCtx.newPage();watch(stale,'stale');
 await stale.evaluate(()=>{});await stale.goto(oldURL);
 // This browser's submission was sent: the page keeps showing it even if the link moved on.
 await stale.waitForFunction(()=>['invalid','submitted'].includes(document.body.dataset.phase));
 const fresh=await browser.newContext({permissions:['camera','microphone'],viewport:{width:390,height:844}});
 const rotated=await fresh.newPage();watch(rotated,'rotated');
 await rotated.goto(oldURL);
 await phase(rotated,'invalid');
 assert.match(await rotated.locator('#rec-notice').innerText(),/doesn’t work any more/);
 await shot('guest-rotated-390',rotated);
 const res=await fetch(`${origin}/api/v1/recordlinks`,{method:'POST',headers:{...admin,'Content-Type':'application/json'},body:JSON.stringify({title:'Short-lived',expiresInHours:1})});
 const short=await res.json();
 await fetch(`${origin}/api/v1/recordlinks/${short.id}`,{method:'PATCH',headers:{...admin,'Content-Type':'application/json'},body:JSON.stringify({expiresAt:new Date(Date.now()-60_000).toISOString()})});
 const expired=await fresh.newPage();watch(expired,'expired');
 await expired.goto(short.url);
 await phase(expired,'expired');
 assert.match(await expired.locator('#rec-notice').innerText(),/expired/);
 await expired.setViewportSize({width:1280,height:800});
 await shot('guest-expired-1280',expired);
 // The 390 px guest green room, on a new link.
 const mobileRes=await fetch(`${origin}/api/v1/recordlinks`,{method:'POST',headers:{...admin,'Content-Type':'application/json'},body:JSON.stringify({title:'Testimonial',prompt:'# Your take\nWhat changed for you after the course?'})});
 const mobileLink=await mobileRes.json();
 const mobile=await fresh.newPage();watch(mobile,'mobile');
 await mobile.goto(mobileLink.url);
 await mobile.waitForFunction(()=>document.querySelector('#rec-preview').videoWidth>0);
 await mobile.locator('#rec-name').fill('Ada');
 await mobile.waitForFunction(()=>document.querySelectorAll('#rec-preflight .rec-check').length>=5);
 const overflow=await mobile.evaluate(()=>document.documentElement.scrollWidth-document.documentElement.clientWidth);
 assert.ok(overflow<=0,`horizontal overflow at 390 px: ${overflow}px`);
 await shot('guest-setup-390',mobile,{fullPage:true});
 log('rotated and expired links refused');

 // 8. Abandoned drafts [SL-P5-4]. Ada records a short take on the new link
 // and leaves without sending; Bo opens a draft and records nothing; Cy has
 // just started. Two days pass for Ada and Bo (their activity is backdated
 // in the daemon's database): the host sees two abandoned drafts with sizes,
 // deletes Bo's, then cleans up the rest. Cy's draft, in progress, is kept
 // and can't be deleted; Grace's sent recording is untouched.
 await mobile.locator('#rec-record').click();
 await phase(mobile,'recording',20000);
 await wait(4000);
 await mobile.locator('#rec-record').click();
 await phase(mobile,'review',30000);
 await mobile.waitForFunction(()=>document.querySelector('.rec-take')?.dataset.uploaded==='true',{},{timeout:60000});
 const adaDraft=JSON.parse(await mobile.evaluate(id=>localStorage.getItem(`studiolink.record.${id}`),mobileLink.id)).submissionId;
 await mobile.close();
 const mobileSecret=new URL(mobileLink.url).searchParams.get('token');
 const startDraft=async name=>(await(await fetch(`${origin}/api/v1/record/${mobileLink.id}/submissions`,{method:'POST',headers:{Authorization:`Bearer ${mobileSecret}`,'Content-Type':'application/json'},body:JSON.stringify({name})})).json()).submission.id;
 const boDraft=await startDraft('Bo'),cyDraft=await startDraft('Cy');
 {
  const {DatabaseSync}=await import('node:sqlite');
  const db=new DatabaseSync(path.join(data,'studiolink.db'));
  db.exec('PRAGMA busy_timeout = 5000');
  const at=new Date(Date.now()-2*86_400_000).toISOString();
  for(const id of [adaDraft,boDraft]){
   db.prepare('UPDATE record_submissions SET created_at = ? WHERE session_id = ?').run(at,id);
   db.prepare('UPDATE takes SET wall_clock_start = ? WHERE session_id = ?').run(at,id);
   db.prepare('UPDATE chunks SET received_at = ? WHERE track_id IN (SELECT t.id FROM tracks t JOIN takes k ON k.id = t.take_id WHERE k.session_id = ?)').run(at,id);
  }
  db.close();
 }
 const draftsApi=async()=>(await(await fetch(`${origin}/api/v1/recordlinks/drafts`,{headers:admin})).json()).drafts;
 const adaBytes=(await draftsApi()).find(d=>d.id===adaDraft).bytes;
 assert.ok(adaBytes>10_000,`Ada's draft holds ${adaBytes} bytes`);
 assert.ok((await readdir(path.join(data,'sessions',adaDraft))).length>0,'Ada draft files');
 await host.setViewportSize({width:1280,height:900});
 await host.reload();
 const abandoned=host.locator('#record-abandoned');
 await abandoned.waitFor({state:'visible'});
 assert.equal(await abandoned.locator('.record-draft').count(),2);
 assert.match(await abandoned.locator(`[data-draft-id="${adaDraft}"]`).innerText(),/Ada[\s\S]*Testimonial · 1 take · [\d.]+ (KB|MB) · last activity 2 days ago/);
 assert.match(await abandoned.locator('.record-abandoned-head').innerText(),/2 drafts/);
 assert.match(await abandoned.locator('.field-hint').innerText(),/14 days without activity/);
 assert.equal(await host.locator(`.record-link:has-text("Testimonial") .record-drafts`).innerText(),'1 recording in progress','only Cy counts as in progress');
 await abandoned.scrollIntoViewIfNeeded();
 await shot('host-abandoned-drafts',host.locator('#recordings-panel'));
 await host.setViewportSize({width:390,height:844});
 assert.ok(await host.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),'abandoned drafts overflow at 390 px');
 await shot('host-abandoned-drafts-390',host.locator('#recordings-panel'));
 await host.setViewportSize({width:1280,height:900});
 // One by hand.
 await abandoned.locator(`[data-draft-id="${boDraft}"] [data-act=delete-draft]`).click();
 await host.locator('.room-dialog [data-confirm]').click();
 await host.locator('#toast-container',{hasText:'Draft deleted'}).waitFor();
 await abandoned.locator(`[data-draft-id="${boDraft}"]`).waitFor({state:'detached'});
 // In progress: refused by the server even when asked directly.
 assert.equal((await fetch(`${origin}/api/v1/recordlinks/drafts/${cyDraft}`,{method:'DELETE',headers:admin})).status,409);
 // The rest at once.
 await abandoned.locator('[data-act=cleanup-drafts]').click();
 assert.match(await host.locator('.room-dialog p').innerText(),/1 draft guests never sent/);
 await host.locator('.room-dialog [data-confirm]').click();
 await host.locator('#toast-container',{hasText:'1 draft deleted'}).waitFor();
 await abandoned.waitFor({state:'hidden'});
 const left=await draftsApi();
 assert.deepEqual(left.map(d=>[d.name,d.abandoned]),[['Cy',false]]);
 await assert.rejects(readdir(path.join(data,'sessions',adaDraft)),/ENOENT/,'Ada draft files survive');
 // Grace's sent recording is untouched.
 assert.equal((await fetch(`${origin}/api/v1/takes/${take2}/originals`,auth)).status,200);
 assert.equal((await links()).find(l=>l.id===link.id).submissions.find(s=>s.id===submissionId)?.status,'submitted');
 log(`abandoned drafts: listed with sizes (${adaBytes} B), one deleted, the rest cleaned up; in-progress draft and sent recording kept`);

 const secretHits=[new URL(oldURL).searchParams.get('token'),new URL(short.url).searchParams.get('token'),JSON.parse(draft).key].filter(s=>logs.includes(s));
 assert.deepEqual(secretHits,[],'record link secret or submission key in the daemon log');
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({recordLink:'ok',takeSec:TAKE_MS/1000,reRecorded:true,uncompressedMic:true,tabKilledMidUpload:true,droppedChunkRequests:blocked,resumed:true,exported:true,editorMedia:media.map(f=>path.basename(f)),originals:originals.length,rotatedRefused:true,expiredRefused:true,secondsTotal:+ts(),errors}));
}catch(e){console.error('errors',JSON.stringify(errors));console.error(logs.slice(-6000));throw e;}
finally{await browser?.close();daemon.kill('SIGTERM');await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);await rm(data,{recursive:true,force:true});}
