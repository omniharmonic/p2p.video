// Phone as an extra camera (PRD F3) [SL-P1-9] against the real daemon with
// Chromium fake devices. Host + guest "Ada"; a take is already recording when
// Ada clicks "Add phone camera" (More menu): the dialog shows a QR code drawn
// on the page and the link. A second browser context with a phone viewport
// and user agent opens the link: it joins as Ada's subsource without an
// admission prompt, appears to everyone as an "Ada · Phone" tile, receives no
// call media (publish-only), registers late in the running take and records.
// The link works once. Stopping the take uploads the phone's file under Ada;
// the export carries it as Ada's "Phone" angle at a positive offset. A second
// take records the phone from the start. Screenshots (phone 390 px, QR dialog)
// go to STUDIO_SCREENSHOT_DIR. Build first; STUDIO_BINARY is the daemon.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,mkdir,readFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,startTake,launchURL,revealControl,readyToJoin} from './lib/harness.mjs';
const {chromium,devices}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-phonecam-'));
const port=Number(process.env.STUDIO_TEST_PORT||18975),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];let browser;
const wait=ms=>new Promise(r=>setTimeout(r,ms));
// 422: invite without Tailscale; 401: the reused phone link (expected).
const expectedConsole=m=>/422|401|HTTP Authentication failed/.test(m.text());
const phoneTile=(page,name='Ada · Phone')=>page.waitForFunction(name=>{const t=[...document.querySelectorAll('#video-grid .tile[data-participant-id]')].find(t=>t.querySelector('.tile-name')?.textContent===name);return !!t&&t.classList.contains('tile-phone')&&t.querySelector('video').videoWidth>0;},name,{timeout:30000});

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async(name,options={viewport:{width:1280,height:720}})=>{
  const context=await browser.newContext({permissions:['camera','microphone'],...options});
  await context.addInitScript(()=>{const O=window.RTCPeerConnection;window.__pcs=[];window.RTCPeerConnection=class extends O{constructor(...a){super(...a);window.__pcs.push(this);}};});
  const page=await context.newPage();page.setDefaultTimeout(45000);
  page.on('console',m=>{if(m.type()==='error'&&!expectedConsole(m))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  return page;
 };
 const host=await open('Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 const ada=await open('Ada');
 await ada.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await ada.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await ada.locator('#name-input').fill('Ada');await ada.locator('#join-btn').click();
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex');
 await host.locator('#admit-btn').click();await ada.locator('#studio-view').waitFor({state:'visible'});
 await host.waitForFunction(()=>document.querySelector('#video-grid .tile[data-participant-id] video')?.videoWidth>0);

 // ---- Take 1 is already recording when the phone is added.
 await startTake(host);
 await ada.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await wait(1500);
 await (await revealControl(ada,'#phone-camera-btn')).click();
 const dialog=ada.locator('#phone-camera-dialog');
 await dialog.locator('[data-qr] svg').waitFor();
 const link=await ada.locator('#phone-link-input').inputValue();
 assert.ok(link.startsWith(`${origin}/phone.html?session=${session.id}&token=`),link);
 assert.match(await dialog.locator('[data-expiry]').innerText(),/Works once · expires in (9:5\d|10:00)/);
 // The QR code decodes to the link where this browser has a barcode detector.
 const decoded=await ada.evaluate(async()=>{if(!('BarcodeDetector' in window))return null;const svg=document.querySelector('#phone-camera-dialog [data-qr] svg');const img=new Image();img.src='data:image/svg+xml,'+encodeURIComponent(svg.outerHTML);await img.decode();const c=document.createElement('canvas');c.width=c.height=600;c.getContext('2d').drawImage(img,0,0,600,600);try{const r=await new BarcodeDetector({formats:['qr_code']}).detect(c);return r[0]?.rawValue??'';}catch{return null;}});
 if(decoded!==null)assert.equal(decoded,link,'QR code payload');
 console.log('phone link minted for Ada',JSON.stringify({qrDecoded:decoded===null?'BarcodeDetector unavailable':decoded===link}));
 await ada.waitForFunction(()=>document.querySelectorAll('.toast').length===0,{},{timeout:15000});
 await screenshot('parity-phone-qr-dialog',ada);

 // ---- The phone opens the link (mobile viewport + UA).
 const phone=await open('Phone',{...devices['iPhone 13'],permissions:['camera','microphone'],viewport:{width:390,height:844}});
 await phone.goto(link);
 assert.ok(!(await phone.evaluate(()=>location.search)).includes('token'),'the capability is dropped from the address bar');
 await phone.waitForFunction(()=>document.body.dataset.state==='recording',{},{timeout:30000});
 assert.equal(await phone.locator('#phone-name').innerText(),'Ada · Phone');
 for(const page of [host,ada])await phoneTile(page);
 await dialog.locator('.phone-status',{hasText:'Phone connected'}).waitFor();
 assert.equal(await host.evaluate(()=>document.querySelector('#host-admission-banner').style.display),'none','no admission prompt for the phone');
 // Publish-only: the phone receives no call video.
 const inbound=await phone.evaluate(async()=>{let n=0;for(const pc of window.__pcs)(await pc.getStats()).forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='video')n+=s.bytesReceived;});return n;});
 assert.equal(inbound,0,'the phone received call video');
 assert.equal(await phone.locator('#phone-flip').isDisabled(),true,'camera switching is locked while recording');
 console.log('phone joined as Ada’s extra source mid-take; tiles labelled; recording late');
 await ada.locator('#phone-camera-dialog button[value=done]').click();
 await screenshot('parity-phone-390-recording',phone);
 // The link works once.
 const replay=await open('Replay',{...devices['iPhone 13'],permissions:['camera','microphone'],viewport:{width:390,height:844}});
 await replay.goto(link);
 await replay.waitForFunction(()=>document.body.dataset.state==='error'&&/already used|expired/.test(document.querySelector('#phone-status').textContent),{},{timeout:20000});
 await replay.context().close();
 console.log('a used phone link is refused');

 // ---- Stop take 1: the phone uploads under Ada.
 await wait(3000);
 await host.locator('#rec-btn').click();
 await phone.waitForFunction(()=>document.body.dataset.state==='done',{},{timeout:60000});
 await screenshot('parity-phone-390-done',phone);
 const headers={Authorization:`Bearer ${session.hostToken}`};
 const takeSnapshot=async n=>{const d=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();const tk=d.takes.find(t=>t.number===n);return tk?await(await fetch(`${origin}/api/v1/takes/${tk.id}`,{headers})).json():null;};
 let snap;
 for(let i=0;i<300;i++){snap=await takeSnapshot(1);if(snap&&snap.tracks.length===3&&snap.tracks.every(t=>t.status==='assembled'))break;await wait(100);}
 assert.equal(snap.tracks.length,3,JSON.stringify(snap.tracks));
 assert.ok(snap.tracks.every(t=>t.status==='assembled'));
 assert.equal(snap.missingParticipants.length,0);
 const phoneTrack=snap.tracks.find(t=>t.sourceLabel==='Phone Camera');
 assert.ok(phoneTrack&&phoneTrack.participantName==='Ada'&&phoneTrack.totalBytes>1000,JSON.stringify(snap.tracks));
 const post=await fetch(`${origin}/api/v1/takes/${snap.take.id}/postprocess`,{method:'POST',headers});
 assert.ok(post.ok,await post.text());
 const out=path.join(data,'sessions',session.id,'takes',snap.take.id,'out');
 const manifest=JSON.parse(await readFile(path.join(out,'manifest.json'),'utf8'));
 const angle=manifest.sources.find(s=>s.source==='phone'&&s.kind==='video');
 assert.ok(angle&&angle.participant==='Ada',JSON.stringify(manifest.sources));
 assert.ok(angle.offsetSec>0.5,`late phone source offset ${angle.offsetSec}`);
 const probe=JSON.parse(execFileSync('ffprobe',['-v','error','-count_frames','-select_streams','v:0','-show_entries','stream=nb_read_frames','-of','json',path.join(out,angle.file)],{encoding:'utf8'}));
 assert.ok(Number(probe.streams[0].nb_read_frames)>10,JSON.stringify(probe));
 const fcpxml=await readFile(path.join(out,'take_01_multicam.fcpxml'),'utf8');
 assert.ok(fcpxml.includes('name="Ada Phone"')&&fcpxml.includes('name="Ada Camera"'),'multicam has Ada’s phone as its own angle');
 console.log('export carries the phone angle',JSON.stringify({offsetSec:angle.offsetSec,durationSec:angle.durationSec,frames:probe.streams[0].nb_read_frames}));
 for(const page of [host,ada])await page.locator('#transfers-recovery-modal').evaluate(m=>m.style.display='none').catch(()=>{});

 // ---- Take 2: the phone records from the start.
 await startTake(host);
 await phone.waitForFunction(()=>document.body.dataset.state==='recording',{},{timeout:20000});
 await wait(2500);
 await host.locator('#rec-btn').click();
 await phone.waitForFunction(()=>document.body.dataset.state==='done',{},{timeout:60000});
 for(let i=0;i<300;i++){snap=await takeSnapshot(2);if(snap&&snap.tracks.length===3&&snap.tracks.every(t=>t.status==='assembled'))break;await wait(100);}
 assert.ok(snap.tracks.some(t=>t.sourceLabel==='Phone Camera'&&t.participantName==='Ada'&&t.status==='assembled'),JSON.stringify(snap.tracks));
 console.log('take 2 recorded the phone from the start');
 assert.ok(await phone.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),'phone page overflows at 390px');
 assert.deepEqual(errors,[]);
 console.log('phone camera browser test passed');
}catch(err){console.error(err);console.error(logs.slice(-4000));process.exitCode=1;}
finally{await browser?.close();daemon.kill();await rm(data,{recursive:true,force:true});}
