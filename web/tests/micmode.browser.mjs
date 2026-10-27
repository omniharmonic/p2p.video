// Recording mic processing mode [SL-P1-10] against the real daemon with
// Chromium fake devices. Measures (never assumes) MediaStreamTrack
// getSettings() of the recording microphone per mode, and that the call
// microphone keeps its own processing (noise suppression toggle included):
//  - green room: auto without headphones = clean; "I'm wearing headphones" = raw;
//    explicit Clean / Raw; back to auto
//  - in the call: raw recording mic + processed call mic; switch to clean from
//    the Devices popover; call noise suppression off; recording untouched
//  - host sees "Raw mic" / "Clean mic (no headphones)" per participant
//  - host session default ("Always raw") overrides auto; changes lock during a
//    take and apply after it
//  - the take's sidecars and manifest.json record the mode (camera + PCM mic)
// Build first; STUDIO_BINARY is the daemon. Prints the measured table.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp,readFile,readdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,startTake,launchURL,revealControl,readyToJoin,openRecordingOptions} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-micmode-'));
const port=Number(process.env.STUDIO_TEST_PORT||19071),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const errors=[];let browser;
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const expectedConsole=m=>/422|Failed to load resource/.test(m.text());
const measured={};

// Recording mic (the local stream's audio) and the call mic (the audio sender), as the browser reports them.
const tracks=page=>page.evaluate(()=>{
 const pick=t=>{if(!t)return null;const s=t.getSettings();return {ec:s.echoCancellation,ns:s.noiseSuppression,agc:s.autoGainControl,id:t.id,state:t.readyState};};
 const view=[...document.querySelectorAll('#local-video,#preview-video')].map(v=>v.srcObject).find(s=>s&&s.getAudioTracks().length);
 const pc=(window.__pcs||[]).filter(p=>p.connectionState==='connected').at(-1);
 const sent=pc?.getSenders().find(s=>s.track?.kind==='audio')?.track;
 return {recording:pick(view?.getAudioTracks()[0]),call:pick(sent),status:(document.querySelector('#recmic-status-call')?.offsetParent?document.querySelector('#recmic-status-call'):document.querySelector('#recmic-status'))?.textContent||''};
});
const proc=t=>t&&{ec:t.ec,ns:t.ns,agc:t.agc};
const RAW={ec:false,ns:false,agc:false},CLEAN={ec:true,ns:true,agc:false},CALL={ec:true,ns:true,agc:true};
async function recordingBecomes(page,want,label){
 const ok=await page.waitForFunction(w=>{const v=[...document.querySelectorAll('#local-video,#preview-video')].map(v=>v.srcObject).find(s=>s&&s.getAudioTracks().length);const t=v?.getAudioTracks()[0];if(!t||t.readyState!=='live')return false;const s=t.getSettings();return s.echoCancellation===w.ec&&s.noiseSuppression===w.ns&&s.autoGainControl===w.agc;},want,{timeout:15000}).then(()=>true).catch(()=>false);
 const now=await tracks(page);
 measured[label]={recording:proc(now.recording),call:proc(now.call)};
 assert.ok(ok,`${label}: recording mic ${JSON.stringify(now.recording)} want ${JSON.stringify(want)}`);
 return now;
}

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 console.log('chromium',browser.version());
 const open=async name=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:800}});
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

 // ---- Green room (guest).
 const guest=await open('Guest');
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#headphones-toggle').waitFor();
 assert.equal(await guest.locator('#headphones-toggle').isChecked(),false,'fake devices carry no headphone-like label');
 await recordingBecomes(guest,CLEAN,'green room: auto, no headphones');
 assert.match((await tracks(guest)).status,/Clean mic \(no headphones\)/);
 await guest.locator('#headphones-toggle').check();
 await recordingBecomes(guest,RAW,'green room: auto, headphones');
 assert.match((await tracks(guest)).status,/Raw mic \(headphones\)/);
 await openRecordingOptions(guest);await guest.locator('#recmic-mode').selectOption('clean');
 await recordingBecomes(guest,CLEAN,'green room: clean chosen');
 await guest.locator('#recmic-mode').selectOption('raw');
 await recordingBecomes(guest,RAW,'green room: raw chosen');
 await guest.locator('#recmic-mode').selectOption('auto');
 await recordingBecomes(guest,RAW,'green room: auto, headphones (again)');
 assert.equal(await guest.evaluate(()=>localStorage.getItem('studiolink.recordingMicMode')),'auto');

 // ---- Join with the uncompressed mic too, so the PCM track's processing is checked.
 await guest.locator('#pcm-audio-toggle').check();
 await guest.locator('#name-input').fill('Guest');await guest.locator('#join-btn').click();
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex');
 await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
 await guest.waitForFunction(()=>(window.__pcs||[]).some(p=>p.connectionState==='connected'));
 let now=await recordingBecomes(guest,RAW,'in call: raw recording');
 assert.deepEqual(proc(now.call),CALL,'the call microphone keeps its own processing next to a raw recording mic');
 assert.notEqual(now.call.id,now.recording.id);

 // Host sees the guest's mode per participant.
 const hostSees=re=>host.waitForFunction(r=>new RegExp(r).test(document.querySelector('#health-participants-list')?.textContent||''),re.source,{timeout:15000});
 await hostSees(/Raw mic \(headphones\)/);
 console.log('host sees: Raw mic (headphones)');

 // Raw recording mic: the call mic's noise suppression toggle changes only the call.
 await (await revealControl(guest,'#devices-btn')).click();
 await guest.locator('#call-audio-noiseSuppression').click();
 await guest.waitForFunction(()=>document.querySelector('#call-audio-noiseSuppression').getAttribute('aria-checked')==='false');
 await guest.waitForFunction(()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);return pc.getSenders().find(s=>s.track?.kind==='audio')?.track.getSettings().noiseSuppression===false;});
 now=await tracks(guest);
 measured['in call: raw + call NS off']={recording:proc(now.recording),call:proc(now.call)};
 assert.deepEqual(proc(now.call),{ec:true,ns:false,agc:true});
 assert.deepEqual(proc(now.recording),RAW,'recording stays raw when the call drops noise suppression');
 await guest.locator('#call-audio-noiseSuppression').click();
 await guest.waitForFunction(()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);return pc.getSenders().find(s=>s.track?.kind==='audio')?.track.getSettings().noiseSuppression===true;});

 // Clean recording mic in the call: Chromium allows one processed capture per
 // microphone, so the call shares the clean capture (EC + NS, no AGC) and its
 // switches show that, disabled; the recording is exactly clean.
 await guest.locator('#recmic-mode-call').selectOption('clean');
 now=await recordingBecomes(guest,CLEAN,'in call: clean chosen');
 await guest.waitForFunction(()=>document.querySelector('.call-audio-processing[data-follows]')?.dataset.follows==='true');
 now=await tracks(guest);
 measured['in call: clean chosen']={recording:proc(now.recording),call:proc(now.call)};
 assert.deepEqual(proc(now.call),CLEAN,'the call shares the clean capture');
 assert.equal(await guest.locator('#call-audio-noiseSuppression').isDisabled(),true,'call switches follow the clean recording mic');
 assert.equal(await guest.locator('#call-audio-autoGainControl').getAttribute('aria-checked'),'false');
 await hostSees(/Clean mic \(chosen\)/);
 // Back to raw: the call gets its own processed capture again.
 await guest.locator('#recmic-mode-call').selectOption('raw');
 now=await recordingBecomes(guest,RAW,'in call: raw again');
 await guest.waitForFunction(()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);return pc.getSenders().find(s=>s.track?.kind==='audio')?.track.getSettings().autoGainControl===true;});
 now=await tracks(guest);
 measured['in call: raw again']={recording:proc(now.recording),call:proc(now.call)};
 assert.deepEqual(proc(now.call),CALL);
 assert.equal(await guest.locator('#call-audio-noiseSuppression').isDisabled(),false);

 // Back to auto without headphones, then the host sets "Always raw" for everyone.
 await guest.locator('#recmic-mode-call').selectOption('auto');
 await guest.locator('#headphones-toggle-call').uncheck();
 await recordingBecomes(guest,CLEAN,'in call: auto, no headphones');
 await hostSees(/Clean mic \(no headphones\)/);
 await guest.keyboard.press('Escape');
 await (await revealControl(host,'#settings-btn')).click();
 await host.locator('#recording-mic-default').selectOption('raw');
 const saved=host.waitForResponse(r=>r.url().includes('/settings')&&r.request().method()==='PATCH');
 await host.locator('#save-settings-btn').click();
 assert.ok((await saved).ok());
 await recordingBecomes(guest,RAW,'host default raw (guest auto)');
 // The call regains its own processed capture once the clean one is gone.
 await guest.waitForFunction(()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);return pc.getSenders().find(s=>s.track?.kind==='audio')?.track.getSettings().autoGainControl===true;},null,{timeout:15000});
 now=await tracks(guest);
 measured['host default raw (guest auto)']={recording:proc(now.recording),call:proc(now.call)};
 assert.deepEqual(proc(now.call),CALL);
 await hostSees(/Raw mic \(host setting\)/);

 // A take locks the mode; a host change during it applies after the take.
 const problems=await startTake(host);
 if(problems)console.log('pre-flight problems confirmed:',problems.replace(/\s+/g,' ').slice(0,200));
 await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await (await revealControl(guest,'#devices-btn')).click();
 assert.equal(await guest.locator('#recmic-mode-call').isDisabled(),true,'mode is locked while recording');
 assert.equal(await guest.locator('#headphones-toggle-call').isDisabled(),true);
 await guest.keyboard.press('Escape');
 const patched=await fetch(`${origin}/api/v1/sessions/${session.id}/settings`,{method:'PATCH',headers:{Authorization:`Bearer ${session.hostToken}`,'Content-Type':'application/json'},body:JSON.stringify({recording_mic_mode:'clean'})});
 assert.ok(patched.ok);
 await wait(1500);
 assert.deepEqual(proc((await tracks(guest)).recording),RAW,'the recording mic never changes mid-take');
 await wait(2000);
 await host.locator('#rec-btn').click();
 await recordingBecomes(guest,CLEAN,'after the take: host default clean applied');

 // The take's sidecars and the export manifest carry the mode the take used (raw, host setting).
 let snapshot;
 for(let i=0;i<120;i++){
  const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
  const take=detail.takes?.[0];
  if(take){snapshot=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();if(snapshot.tracks.length===3&&snapshot.tracks.every(t=>t.status==='assembled'))break;}
  await wait(500);
 }
 assert.equal(snapshot?.tracks.length,3,'host camera + guest camera + guest PCM: '+JSON.stringify(snapshot));
 const raw=path.join(data,'sessions',session.id,'takes',String(snapshot.take.number),'raw');
 const rawDir=await readdir(raw).then(()=>raw).catch(()=>path.join(data,'sessions',session.id,'takes',snapshot.take.id,'raw'));
 const sidecars={};
 for(const t of snapshot.tracks)sidecars[t.trackId]=JSON.parse(await readFile(path.join(rawDir,t.trackId+'.sidecar.json'),'utf8'));
 const guestTracks=snapshot.tracks.filter(t=>t.participantName==='Guest');
 assert.equal(guestTracks.length,2);
 for(const t of guestTracks){
  const m=sidecars[t.trackId].micProcessing;
  measured[`take sidecar: ${t.kind} (${t.sourceLabel})`]=m;
  assert.equal(m?.resolved,'raw',`${t.sourceLabel} sidecar: ${JSON.stringify(m)}`);
  assert.equal(m.reason,'session');assert.equal(m.echoCancellation,false);assert.equal(m.noiseSuppression,false);assert.equal(m.autoGainControl,false);
 }
 const pcm=guestTracks.find(t=>t.kind==='audio');
 const pcmSettings=sidecars[pcm.trackId].pcm?.sourceSettings;
 assert.equal(pcmSettings?.echoCancellation,false,'PCM mic opened raw like the recording mic');
 const hostCam=snapshot.tracks.find(t=>t.participantName==='Host');
 assert.ok(sidecars[hostCam.trackId].micProcessing,'host camera sidecar has its mode too');
 const post=await fetch(`${origin}/api/v1/takes/${snapshot.take.id}/postprocess`,{method:'POST',headers:{Authorization:`Bearer ${session.hostToken}`}});
 if(post.ok){
  const outRoot=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'out');
  const manifest=JSON.parse(await readFile(path.join(outRoot,'manifest.json'),'utf8'));
  const guestAudio=manifest.sources.filter(s=>s.participant==='Guest'&&s.kind==='audio');
  assert.ok(guestAudio.length>=1&&guestAudio.every(s=>s.micProcessing?.resolved==='raw'),'manifest audio: '+JSON.stringify(guestAudio));
  assert.ok(manifest.sources.filter(s=>s.kind==='video').every(s=>!s.micProcessing),'video sources carry no mic processing');
  console.log('manifest.json records micProcessing on',guestAudio.map(s=>s.id).join(', '));
 }else console.log('export not run (ffmpeg unavailable?):',post.status,(await post.text()).slice(0,160));

 console.table(Object.fromEntries(Object.entries(measured).map(([k,v])=>[k,v&&v.recording?{recording:JSON.stringify(v.recording),call:JSON.stringify(v.call)}:{recording:JSON.stringify(v),call:''}])));
 assert.deepEqual(errors,[]);
 console.log('micmode browser test passed');
}catch(err){console.error(err);console.error(JSON.stringify(measured,null,1));console.error(logs.slice(-3000));process.exitCode=1;}
finally{await browser?.close();daemon.kill();}
