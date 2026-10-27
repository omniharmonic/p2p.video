// Background effects [SL-P1-7] and call audio processing [SL-P1-8] against the
// real daemon (served with its production CSP) and Chromium fake devices fed a
// moving 20 px checkerboard (ffmpeg), so every region has texture that the
// call encoder keeps and a blur removes. STUDIO_HEADLESS_CHANNEL=chromium runs
// Chrome's new headless mode (real GPU on a Mac) for the fps/CPU numbers.
//
// Verifies: the segmentation runtime loads from our origin with no CSP
// violation; the green room preview shows the effect and the choice persists
// across a reload; in a call the remote receiver decodes a blurred host (edge
// energy in a corner region far below the raw camera's) while the host's
// recorded file keeps the sharp original; a generated background replaces the
// frame; processed output holds ≥ 20 fps headless; turning effects off sends
// the raw camera again; CPU throttling that collapses the frame rate turns the
// effect off with a notice. Call audio: the recording mic is raw while the
// call sends a processed capture; noise suppression off changes the call
// sender's track only. Screenshots go to
// STUDIO_SCREENSHOT_DIR. Build first; STUDIO_BINARY is the daemon.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,mkdir,readdir,stat} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,startTake,launchURL,revealControl,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-effects-'));
const port=Number(process.env.STUDIO_TEST_PORT||18961),origin=`http://127.0.0.1:${port}`;
// A textured, moving camera: 1280×720 checkerboard, 4 s loop.
const y4m=path.join(data,'pattern.y4m');
execFileSync('ffmpeg',['-v','error','-f','lavfi','-i','color=c=gray:s=1280x720:r=30','-vf',"geq=lum='if(mod(floor((X+N*4)/20)+floor(Y/20),2),220,30)':cb=128:cr=128",'-t','4','-pix_fmt','yuv420p',y4m]);
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];let browser;
const wait=ms=>new Promise(r=>setTimeout(r,ms));
// MediaPipe prints its XNNPACK INFO line through stderr (console.error).
const expectedConsole=m=>/422|XNNPACK delegate/.test(m.text());

// Mean absolute horizontal+vertical gradient of a gray image region: blur drives it down.
const edgeEnergy=(gray,w,h,region)=>{const [x0,y0,x1,y1]=region.map((v,i)=>Math.round(v*(i%2?h:w)));let sum=0,n=0;for(let y=y0;y<y1-1;y++)for(let x=x0;x<x1-1;x++){const i=y*w+x;sum+=Math.abs(gray[i]-gray[i+1])+Math.abs(gray[i]-gray[i+w]);n++;}return sum/n;};
const CORNER=[0,0,0.35,0.35];
// Grab a 320×180 gray frame of a <video> in the page.
const grabGray=(page,selector)=>page.evaluate(sel=>{const v=document.querySelector(sel);const c=document.createElement('canvas');c.width=320;c.height=180;const x=c.getContext('2d');x.drawImage(v,0,0,320,180);const d=x.getImageData(0,0,320,180).data;const g=new Array(320*180);for(let i=0;i<g.length;i++)g[i]=Math.round(d[i*4]*.299+d[i*4+1]*.587+d[i*4+2]*.114);return g;},selector);
const frameRate=(page,selector,ms=3000)=>page.evaluate(([sel,ms])=>new Promise(res=>{const v=document.querySelector(sel);let n=0;const t0=performance.now();const step=()=>{n++;if(performance.now()-t0<ms)v.requestVideoFrameCallback(step);else res(n*1000/(performance.now()-t0));};v.requestVideoFrameCallback(step);}),[selector,ms]);
// %CPU of this test's browser and all its child processes (renderers, GPU, utility).
// The browser is found by its Playwright profile directory (unique per launch).
let profileMarker='';
const chromiumCpu=()=>{try{const lines=execFileSync('ps',['-A','-o','pid=,ppid=,%cpu=,command='],{encoding:'utf8'}).trim().split('\n');const rows=lines.map(l=>l.trim().split(/\s+/,3).map(Number));const roots=lines.filter(l=>profileMarker&&l.includes(profileMarker)&&!l.includes('--type=')).map(l=>Number(l.trim().split(/\s+/)[0]));const tree=new Set(roots);let grew=true;while(grew){grew=false;for(const [pid,ppid] of rows)if(tree.has(ppid)&&!tree.has(pid)){tree.add(pid);grew=true;}}return rows.filter(r=>tree.has(r[0])).reduce((s,r)=>s+r[2],0);}catch{return NaN;}};

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 const csp=(await fetch(origin+'/')).headers.get('content-security-policy');
 browser=await chromium.launch({headless:true,...(process.env.STUDIO_HEADLESS_CHANNEL?{channel:process.env.STUDIO_HEADLESS_CHANNEL}:{}),executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream',`--use-file-for-fake-video-capture=${y4m}`,'--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async(name,viewport={width:1280,height:720})=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport});
  await context.addInitScript(()=>{window.__csp=[];document.addEventListener('securitypolicyviolation',e=>window.__csp.push(`${e.violatedDirective} ${e.blockedURI}`));const O=window.RTCPeerConnection;window.__pcs=[];window.RTCPeerConnection=class extends O{constructor(...a){super(...a);window.__pcs.push(this);}};});
  const page=await context.newPage();page.setDefaultTimeout(45000);
  page.on('console',m=>{if(m.type()==='error'&&!expectedConsole(m))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  return {page,context};
 };
 const {page:host}=await open('Host');
 await host.goto(await launchURL(data,origin));
 await readyToJoin(host);
 profileMarker=(execFileSync('ps',['-A','-o','command='],{encoding:'utf8'}).split('\n').find(l=>l.includes(y4m)&&!l.includes('--type='))?.match(/--user-data-dir=(\S+)/)?.[1])||'';
 await wait(3000);const cpuOff=chromiumCpu();

 // ---- Green room: strong blur on the preview.
 await readyToJoin(host);await host.locator('#ready-bg-btn').click();
 await host.locator('#effect-green-blur-strong').click();
 await host.waitForFunction(()=>{const t=document.querySelector('#preview-video').srcObject?.getVideoTracks()[0];return t&&!/fake|file|y4m/i.test(t.label)&&document.querySelector('#effect-green-blur-strong').getAttribute('aria-checked')==='true';},{},{timeout:60000});
 await wait(2500);
 const previewFps=await frameRate(host,'#preview-video');
 const cpuOn=chromiumCpu();
 const perf={...await host.evaluate(()=>({...document.querySelector('.effects-row').dataset}))};
 assert.deepEqual(await host.evaluate(()=>window.__csp),[],'CSP violations while loading effects');
 console.log('green room blur on',JSON.stringify({previewFps:Math.round(previewFps*10)/10,browserCpuPctOff:Math.round(cpuOff),browserCpuPctOn:Math.round(cpuOn),...perf}));
 assert.ok(previewFps>=20,`processed preview ${previewFps} fps`);
 await screenshot('parity-effects-greenroom',host);
 // Persisted: a reload keeps strong blur.
 await host.reload();
 await readyToJoin(host);
 await host.waitForFunction(()=>document.querySelector('#effect-green-blur-strong')?.getAttribute('aria-checked')==='true'&&!/fake|file|y4m/i.test(document.querySelector('#preview-video').srcObject?.getVideoTracks()[0]?.label??'fake'),{},{timeout:60000});
 console.log('effect choice persisted across reload');

 // ---- Call: host (blur) + guest.
 await readyToJoin(host);await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 const {page:guest}=await open('Guest');
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 // Headphones on: a raw recording mic, so the call mic's processing is fully its own [SL-P1-10].
 await guest.locator('#headphones-toggle').check();
 await guest.waitForFunction(()=>document.querySelector('#preview-video').srcObject?.getAudioTracks()[0]?.getSettings().echoCancellation===false);
 await guest.locator('#name-input').fill('Guest');await guest.locator('#join-btn').click();
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex');
 await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
 const remote='#video-grid .tile[data-participant-id] video';
 await guest.waitForFunction(sel=>document.querySelector(sel)?.videoWidth>0,remote);
 await host.waitForFunction(sel=>document.querySelector(sel)?.videoWidth>0,remote);
 // The host sends the processed track; the raw camera stays in the capture stream.
 const senderInfo=await host.evaluate(()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);const s=pc.getSenders().find(s=>s.track?.kind==='video');return {label:s.track.label,settings:s.track.getSettings()};});
 assert.ok(!/fake|file|y4m/i.test(senderInfo.label),'call sender carries the processed track: '+JSON.stringify(senderInfo));
 await wait(3000);
 // Raw camera reference: the guest's own (unprocessed) preview of the same pattern.
 const raw=edgeEnergy(await grabGray(guest,'#local-video'),320,180,CORNER);
 const sent=edgeEnergy(await grabGray(guest,remote),320,180,CORNER);
 const remoteFps=await frameRate(guest,remote);
 console.log('remote decodes blurred host',JSON.stringify({rawCornerEdge:raw.toFixed(2),sentCornerEdge:sent.toFixed(2),remoteFps:Math.round(remoteFps*10)/10}));
 assert.ok(sent<raw*0.5,`background region not blurred: raw ${raw} vs sent ${sent}`);
 assert.ok(remoteFps>=20,`remote receives ${remoteFps} fps`);

 // ---- Recording keeps the untouched camera.
 await startTake(host);
 await host.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await wait(3500);
 await host.locator('#rec-btn').click();
 const headers={Authorization:`Bearer ${session.hostToken}`};let snapshot;
 for(let i=0;i<300;i++){const d=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();if(d.takes[0]){snapshot=await(await fetch(`${origin}/api/v1/takes/${d.takes[0].id}`,{headers})).json();if(snapshot.tracks.length>=2&&snapshot.tracks.every(t=>t.status==='assembled'))break;}await wait(100);}
 const hostTrack=snapshot.tracks.find(t=>t.participantName==='Host'&&t.kind==='video');
 assert.ok(hostTrack,JSON.stringify(snapshot));
 const rawDir=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'raw');
 const file=(await readdir(rawDir)).find(f=>f.startsWith(hostTrack.trackId+'.')&&/\.(webm|mp4)$/.test(f));
 const frame=execFileSync('ffmpeg',['-v','error','-ss','1.5','-i',path.join(rawDir,file),'-frames:v','1','-vf','scale=320:180','-f','rawvideo','-pix_fmt','gray','-'],{maxBuffer:1<<22});
 const recorded=edgeEnergy([...frame],320,180,CORNER);
 const probe=JSON.parse(execFileSync('ffprobe',['-v','error','-select_streams','v:0','-show_entries','stream=width,height','-of','json',path.join(rawDir,file)],{encoding:'utf8'})).streams[0];
 console.log('recording keeps the original camera',JSON.stringify({recordedCornerEdge:recorded.toFixed(2),sentCornerEdge:sent.toFixed(2),recorded:`${probe.width}x${probe.height}`}));
 assert.ok(recorded>sent*2,`recorded frame looks processed: ${recorded} vs sent ${sent}`);
 assert.ok(probe.width>=1280,'recording is full resolution, not the ≤720p call track');
 await host.locator('#transfers-recovery-modal').evaluate(m=>m.style.display='none').catch(()=>{});
 await guest.locator('#transfers-recovery-modal').evaluate(m=>m.style.display='none').catch(()=>{});

 // ---- In-call picker: generated background, then off.
 await (await revealControl(host,'#effects-btn')).click();
 await host.locator('#effects-panel').waitFor({state:'visible'});
 await host.locator('#effect-call-bg-dusk').click();
 await host.waitForFunction(()=>document.querySelector('#effect-call-bg-dusk').getAttribute('aria-checked')==='true');
 await wait(2500);
 const mean=await guest.evaluate(sel=>{const v=document.querySelector(sel);const c=document.createElement('canvas');c.width=64;c.height=36;const x=c.getContext('2d');x.drawImage(v,0,0,64,36);const d=x.getImageData(0,0,16,10).data;let r=0,g=0,b=0;for(let i=0;i<d.length;i+=4){r+=d[i];g+=d[i+1];b+=d[i+2];}const n=d.length/4;return [r/n,g/n,b/n].map(Math.round);},remote);
 console.log('dusk background corner colour at the receiver',JSON.stringify(mean));
 // Top-left of the dusk gradient is a deep violet (#2a2140): dark, blue ≥ green.
 assert.ok(mean[0]<110&&mean[2]>=mean[1]-8,`corner is not the generated background: ${mean}`);
 for(const page of [host,guest])await page.waitForFunction(()=>document.querySelectorAll('.toast').length===0,{},{timeout:15000});
 await screenshot('parity-effects-picker',host);
 await host.setViewportSize({width:390,height:844});
 assert.ok(await host.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),'picker overflows at 390px');
 await screenshot('parity-effects-picker-390',host);
 await host.setViewportSize({width:1280,height:720});
 await host.locator('#effect-call-off').click();
 await host.waitForFunction(()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);return /fake|file|y4m/i.test(pc.getSenders().find(s=>s.track?.kind==='video').track.label);});
 await wait(2500);
 const offEdge=edgeEnergy(await grabGray(guest,remote),320,180,CORNER);
 console.log('effects off: raw camera sent again',JSON.stringify({cornerEdge:offEdge.toFixed(2)}));
 assert.ok(offEdge>sent*2,`effect still applied after Off: ${offEdge}`);
 await host.keyboard.press('Escape');

 // ---- Collapse: heavy CPU throttling turns the effect off with a notice.
 await host.locator('#effects-panel').evaluate(p=>p.hidden=true);
 await (await revealControl(host,'#effects-btn')).click();
 await host.locator('#effect-call-blur-light').click();
 await host.waitForFunction(()=>document.querySelector('#effect-call-blur-light').getAttribute('aria-checked')==='true');
 await host.keyboard.press('Escape');
 const cdp=await host.context().newCDPSession(host);
 await cdp.send('Emulation.setCPUThrottlingRate',{rate:40});
 const collapsed=await host.waitForFunction(()=>[...document.querySelectorAll('.toast')].some(t=>/couldn’t keep up/.test(t.textContent)),{},{timeout:40000}).then(()=>true).catch(()=>false);
 await cdp.send('Emulation.setCPUThrottlingRate',{rate:1});
 console.log(collapsed?'fps collapse turned the effect off with a notice':'CPU throttling did not collapse the frame rate within 40 s (not asserted)');
 if(collapsed)assert.equal(await host.locator('#effect-call-off').getAttribute('aria-checked'),'true');

 // ---- Call audio: raw recording mic (headphones on, SL-P1-10), processed call mic;
 // then noise suppression off for the call only.
 const audioState=()=>guest.evaluate(()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);const sent=pc.getSenders().find(s=>s.track?.kind==='audio').track;const rec=document.querySelector('#local-video').srcObject.getAudioTracks()[0];const pick=t=>{const s=t.getSettings();return {ns:s.noiseSuppression,ec:s.echoCancellation,agc:s.autoGainControl};};return {sent:pick(sent),recording:pick(rec),sentLive:sent.readyState,distinct:sent.id!==rec.id};});
 const before=await audioState();
 console.log('call audio at join',JSON.stringify(before));
 assert.deepEqual(before.recording,{ns:false,ec:false,agc:false},'the recording microphone is raw (headphones)');
 assert.deepEqual(before.sent,{ns:true,ec:true,agc:true},'the call microphone is processed');
 await (await revealControl(guest,'#devices-btn')).click();
 await guest.locator('#call-audio-noiseSuppression').click();
 await guest.waitForFunction(()=>document.querySelector('#call-audio-noiseSuppression').getAttribute('aria-checked')==='false');
 const audio=await audioState();
 console.log('noise suppression off for the call',JSON.stringify(audio));
 assert.deepEqual(audio.sent,{ns:false,ec:true,agc:true});assert.equal(audio.sentLive,'live');assert.ok(audio.distinct);
 assert.deepEqual(audio.recording,{ns:false,ec:false,agc:false},'the recording microphone is untouched');
 assert.equal(await guest.evaluate(()=>localStorage.getItem('studiolink.callAudioProcessing')),JSON.stringify({noiseSuppression:false,echoCancellation:true,autoGainControl:true}));
 // Host audio still reaches the guest and vice versa after the swap.
 await wait(1500);
 const bytes=()=>host.evaluate(async()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);let t=0;(await pc.getStats()).forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='audio')t+=s.bytesReceived;});return t;});
 const b0=await bytes();await wait(1500);assert.ok(await bytes()>b0+1000,'guest audio stopped after switching call processing');
 await screenshot('parity-call-audio',guest);
 assert.deepEqual(await host.evaluate(()=>window.__csp),[],'CSP violations');
 assert.deepEqual(errors,[]);
 console.log('effects browser test passed',JSON.stringify({csp}));
}catch(err){console.error(err);console.error(logs.slice(-3000));process.exitCode=1;}
finally{await browser?.close();daemon.kill();await rm(data,{recursive:true,force:true});void stat;}
