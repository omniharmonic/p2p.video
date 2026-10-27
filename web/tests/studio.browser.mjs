// Real UI + host integration with isolated storage and Chromium fake devices only.
// Build the web assets and daemon first; STUDIO_BINARY points to that daemon.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,readdir,mkdir,readFile,writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
import {isolatedNetworkArgs,startTake,revealControl,launchURL,readyToJoin,openRecordingOptions} from './lib/harness.mjs';
import {connectFakeTranscriber} from './lib/fake-transcription-provider.mjs';
const data=await mkdtemp(path.join(tmpdir(),'studio-browser-'));
const port=Number(process.env.STUDIO_TEST_PORT||17443),origin=`http://127.0.0.1:${port}`;
// Post-take transcripts [SL-P4-3]: the default Built-in engine is exercised with a fake
// provider speaking the Mac app's protocol (tests/lib/fake-transcription-provider.mjs), and
// the optional Minutes engine with a stand-in minutes-cli honouring the transcribe-file
// contract (docs/integrations/MINUTES.md). Real Apple recognition is covered by the opt-in
// Swift test (STUDIOLINK_REAL_SPEECH=1), since Chromium's fake microphones carry tones, not speech.
const minutesFixture=fileURLToPath(new URL('../../testdata/integrations/minutes.file-transcript.v1.json',import.meta.url));
const minutesCli=path.join(data,'stand-in-minutes-cli');
await writeFile(minutesCli,`#!/bin/sh\nif [ "$1" = help ]; then echo "minutes-cli transcribe-file <audio-path>"; exit 0; fi\nwhile [ $# -gt 0 ]; do if [ "$1" = --output ]; then cp '${minutesFixture}' "$2"; fi; shift; done\n`,{mode:0o755});
const daemon=spawn(process.env.STUDIO_BINARY||'/tmp/studiolink-audit-daemon',['--data-dir',data,'--port',String(port),'--no-funnel','--minutes-cli',minutesCli,...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='',browser,host,guest;
const errors=[];
const pcmEnabled=process.env.STUDIO_TEST_PCM==='1';
const expectedTrackCount=pcmEnabled?4:2;
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page=host,fullPage=true)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),fullPage,animations:'disabled'});};
daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await new Promise(r=>setTimeout(r,100));}assert.ok(ready,'daemon did not start: '+logs);console.log('daemon ready');
 // Opt-in real speech: STUDIO_FAKE_AUDIO_FILE (a WAV, e.g. from `say -o x.aiff` + afconvert) becomes every fake microphone.
 const fakeAudio=process.env.STUDIO_FAKE_AUDIO_FILE?[`--use-file-for-fake-audio-capture=${process.env.STUDIO_FAKE_AUDIO_FILE}`]:[];
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required',...fakeAudio]});
 const blockedContext=await browser.newContext();
 await blockedContext.addInitScript(()=>{navigator.mediaDevices.getUserMedia=async()=>{throw new DOMException('Permission denied for integration test','NotAllowedError')};});
 const blocked=await blockedContext.newPage();await blocked.goto(await launchURL(data,origin));await readyToJoin(blocked,{camera:false});
 await blocked.getByText(/Camera or microphone unavailable: Permission denied for integration test/).waitFor();
 assert.ok(await blocked.locator('#join-btn').isDisabled());
 await screenshot('00-permission-denied',blocked);await blockedContext.close();
 const hostContext=await browser.newContext({permissions:['camera','microphone','clipboard-read','clipboard-write']});
 const guestContext=await browser.newContext({permissions:['camera','microphone']});
 host=await hostContext.newPage();guest=await guestContext.newPage();
 for (const p of [host,guest]) { p.setDefaultTimeout(45000);p.setDefaultNavigationTimeout(15000);p.on('console',m=>{if(m.type()==='error'&&!(m.text().includes('422')&&m.location().url.endsWith('/invite')))errors.push(`console: ${m.text()} ${m.location().url}`);}); }
 for(const p of [host,guest])p.on('pageerror',e=>errors.push(e.message));
 await host.goto(await launchURL(data,origin));console.log("host loaded");await readyToJoin(host);if(pcmEnabled){await openRecordingOptions(host);await host.locator('#pcm-audio-toggle').check();}await screenshot('01-green-room');
 await host.setViewportSize({width:390,height:844});await screenshot('10-green-room-mobile');
 assert.ok(await host.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth));
 await host.setViewportSize({width:1280,height:720});
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST',{timeout:45000});
 await host.locator('#join-btn').click();console.log('host join clicked');const session=await (await created).json();
 assert.ok(session.hostToken);await host.locator('#studio-view').waitFor({state:'visible'});console.log('host admitted');
 await (await revealControl(host,'#settings-btn')).click();
 await host.locator('#settings-drawer-panel.open').waitFor({state:'visible'});
 await host.waitForFunction(()=>document.querySelectorAll('.toast').length===0);
 await screenshot('09-settings',host,false);
 assert.ok(await host.locator('[data-engine=webcodecs]').isDisabled());
 const settingsSaved=host.waitForResponse(response=>response.url().endsWith('/settings')&&response.request().method()==='PATCH');
 await host.locator('#save-settings-btn').click();assert.ok((await settingsSaved).ok());
 await host.waitForFunction(()=>!document.querySelector('#settings-drawer-panel').classList.contains('open'));
 // The production invite UI must refuse localhost; never copy the privileged host URL.
 await host.locator('#copy-invite-btn').click();await host.getByText(/Configure --public-url/).waitFor();
 // Same-machine test guest uses the guest capability from session creation.
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);await guest.locator('#name-input').fill('Guest');if(pcmEnabled){await openRecordingOptions(guest);await guest.locator('#pcm-audio-toggle').check();}await guest.locator('#join-btn').click();console.log('guest requested');
 await host.locator('#admit-btn').waitFor({state:'visible'});await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});console.log('guest admitted');
 assert.ok(await host.locator('#camera-select').isDisabled());assert.ok(await guest.locator('#mic-select').isDisabled());
 for(const id of ['copy-invite-btn','settings-btn','rec-btn','chirp-btn','postproc-btn'])assert.equal(await guest.locator('#'+id).isVisible(),false,id+' exposed to guest');
 for(const p of [host,guest])await p.waitForFunction(()=>[...document.querySelectorAll('[id^="remote-"]')].some(v=>v.tagName==='VIDEO'&&v.videoWidth>0),{},{timeout:20000});
 await host.waitForFunction(()=>document.querySelector('#conn-text').textContent==='Remote video decoded');
 await host.waitForFunction(()=>[...document.querySelectorAll('.tile[data-participant-id] .tile-name')].some(label=>label.textContent==='Guest'));
 await guest.waitForFunction(()=>[...document.querySelectorAll('.tile[data-participant-id] .tile-name')].some(label=>label.textContent==='Host'));
 await (await revealControl(host,host.getByRole('button',{name:'Focus',exact:true}))).click();
 assert.ok(await host.locator('#video-grid').evaluate(grid=>grid.classList.contains('layout-hero')));
 const remoteTile=host.locator('.tile[data-participant-id]').first();const remoteId=await remoteTile.getAttribute('id');
 await remoteTile.click();assert.equal(await host.locator('#video-grid > .tile').first().getAttribute('id'),remoteId);
 await (await revealControl(host,host.getByRole('button',{name:'Switcher',exact:true}))).click();await remoteTile.click();
 assert.ok(await remoteTile.evaluate(tile=>tile.classList.contains('tally-program')));
 await (await revealControl(host,host.getByRole('button',{name:'Grid',exact:true}))).click();
 assert.ok(await host.locator('#video-grid').evaluate(grid=>grid.classList.contains('layout-split')));
 // Return the local source to the first preview slot after testing manual layout selection.
 await host.locator('#video-grid').evaluate(grid=>grid.prepend(document.querySelector('#local-tile')));

 await host.waitForFunction(()=>document.querySelectorAll('.toast').length===0);
 await screenshot('02-live-studio');await screenshot('03-guest-studio',guest);
 await host.setViewportSize({width:390,height:844});await screenshot('11-live-mobile');
 assert.ok(await host.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth));
 await host.setViewportSize({width:1280,height:720});
 console.log('video decoded');
 if(process.env.STUDIO_TEST_STORAGE_FAILURE==='1'||process.env.STUDIO_TEST_PCM_FAILURE==='1'){
  if(process.env.STUDIO_TEST_PCM_FAILURE==='1'){
   assert.equal(pcmEnabled,true);
   await host.evaluate(()=>{const original=navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);navigator.mediaDevices.getUserMedia=async constraints=>{if(constraints.audio&&typeof constraints.audio==='object'&&constraints.audio.sampleRate)throw new DOMException('Injected separate PCM microphone denial','NotAllowedError');return original(constraints);};});
  }else await host.evaluate(()=>{navigator.storage.getDirectory=async()=>{await new Promise(resolve=>setTimeout(resolve,500));throw new DOMException('Injected host OPFS exhaustion','QuotaExceededError');};});
  await startTake(host,{expectRecording:false});
  if(process.env.STUDIO_TEST_STORAGE_FAILURE==='1')await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
  await host.waitForFunction(()=>document.querySelector('#telemetry-status').textContent.includes('Recording failed'));
  let snapshot;
  for(let attempt=0;attempt<50;attempt++){
   const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
   if(detail.takes[0])snapshot=await(await fetch(`${origin}/api/v1/takes/${detail.takes[0].id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
   if(snapshot&&snapshot.take.status!=='recording')break;
   await new Promise(resolve=>setTimeout(resolve,100));
  }
  assert.ok(snapshot);assert.notEqual(snapshot.take.status,'recording','host storage failure left the shared take recording');
  await guest.waitForFunction(()=>document.querySelector('#take-status-badge').textContent.includes('stopped'));
  assert.equal(await guest.locator('#rec-indicator').isVisible(),false);
  assert.match(await host.locator('#telemetry-status').innerText(),/Recording failed/);
  await screenshot('08-host-storage-failure');
  const blocked=await fetch(`${origin}/api/v1/takes/${snapshot.take.id}/postprocess`,{method:'POST',headers:{Authorization:`Bearer ${session.hostToken}`}});
  assert.equal(blocked.ok,false);assert.match(await blocked.text(),/missing|awaiting|empty|recording/i);
  assert.deepEqual(errors,[]);console.log(JSON.stringify({injectedHostStorageFailure:process.env.STUDIO_TEST_STORAGE_FAILURE==='1',injectedPcmMicrophoneFailure:process.env.STUDIO_TEST_PCM_FAILURE==='1',sharedTakeStopped:true,guestStopped:true,incompleteExportBlocked:true,errors}));
 }else{
 await startTake(host);for(const p of [host,guest])await p.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await screenshot('04-recording');
 await new Promise(r=>setTimeout(r,3500));
 if(process.env.STUDIO_TEST_PCM_CRASH==='1'){
  assert.equal(pcmEnabled,true);
  const committed=await guest.evaluate(async()=>{
   const root=await navigator.storage.getDirectory(),takes=await root.getDirectoryHandle('takes');
   for await(const [,take]of takes.entries()){
    const tracks=await take.getDirectoryHandle('tracks');
    for await(const [,track]of tracks.entries()){
     const meta=JSON.parse(await(await(await track.getFileHandle('meta.json')).getFile()).text());
     if(meta.kind==='audio'&&!meta.isComplete)return meta;
    }
   }
  });
  assert.ok(committed?.headerPending);assert.ok(committed.lastSeq>=2);
  await guest.close();await host.locator('#rec-btn').click();
  await host.locator('#transfers-recovery-modal').waitFor({state:'visible'});
  guest=await guestContext.newPage();guest.setDefaultTimeout(45000);guest.on('pageerror',error=>errors.push(error.message));
  await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
  // Reopening the invite resumes unfinished local uploads automatically.
  await guest.locator('#opfs-recovery-banner').waitFor({state:'visible'});
  let snapshot;for(let attempt=0;attempt<150;attempt++){
   snapshot=await(await fetch(`${origin}/api/v1/takes/${committed.takeId}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
   if(snapshot.tracks.find(track=>track.trackId===committed.trackId)?.status==='partial')break;
   await new Promise(resolve=>setTimeout(resolve,100));
  }
  assert.equal(snapshot.tracks.find(track=>track.trackId===committed.trackId)?.status,'partial',JSON.stringify(snapshot));
  const raw=path.join(data,'sessions',session.id,'takes',committed.takeId,'raw',committed.trackId+'.wav');
  const sidecar=JSON.parse(await readFile(raw.replace('.wav','.sidecar.json'),'utf8'));assert.equal(sidecar.captureComplete,false);
  const info=JSON.parse(execFileSync('ffprobe',['-v','error','-show_streams','-of','json',raw],{encoding:'utf8'}));assert.equal(info.streams[0].codec_name,'pcm_f32le');
  assert.ok(Math.abs(Number(info.streams[0].duration)-sidecar.pcm.frames/sidecar.pcm.sampleRate)<.001);execFileSync('ffmpeg',['-v','error','-i',raw,'-f','null','-'],{stdio:'pipe'});
  await guest.getByText(/partial audio prefix was preserved/).waitFor();await screenshot('12-pcm-prefix-recovery',guest);
  const blocked=await fetch(`${origin}/api/v1/takes/${committed.takeId}/postprocess`,{method:'POST',headers:{Authorization:`Bearer ${session.hostToken}`}});assert.equal(blocked.ok,false);
  assert.deepEqual(errors,[]);console.log(JSON.stringify({pcmCrashPrefixRecovered:true,frames:sidecar.pcm.frames,sampleRate:sidecar.pcm.sampleRate,channels:sidecar.pcm.channels,hostStatus:'partial',decoded:true,exportBlocked:true,errors}));
 }else{
 await host.locator('#rec-btn').click();
 await host.locator('#transfers-recovery-modal').waitFor({state:'visible'});
 await screenshot('05-transfers');
 const token=session.hostToken;
 let snapshot;for(let i=0;i<150;i++){
  const detail=await (await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${token}`}})).json();
  const take=detail.takes[0];if(take){snapshot=await (await fetch(`${origin}/api/v1/takes/${take.id}`,{headers:{Authorization:`Bearer ${token}`}})).json();if(snapshot.tracks.length===expectedTrackCount&&snapshot.tracks.every(t=>t.status==='assembled'))break;}
  await new Promise(r=>setTimeout(r,100));
 }
 assert.equal(snapshot.tracks.length,expectedTrackCount,JSON.stringify(snapshot));assert.ok(snapshot.tracks.every(t=>t.status==='assembled'&&t.totalBytes>1000),JSON.stringify(snapshot));
 const raw=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'raw');const media=(await readdir(raw)).filter(f=>f.endsWith('.webm')||f.endsWith('.mp4')||f.endsWith('.wav'));
 assert.equal(media.length,expectedTrackCount);
 for(const file of media){const info=JSON.parse(execFileSync('ffprobe',['-v','error','-show_streams','-of','json',path.join(raw,file)],{encoding:'utf8'}));assert.ok(info.streams.some(s=>s.codec_type==='audio'));if(file.endsWith('.wav')){const audio=info.streams.find(s=>s.codec_type==='audio');assert.equal(audio.codec_name,'pcm_f32le');assert.equal(Number(audio.sample_rate),48000);assert.ok([1,2].includes(audio.channels));}else assert.ok(info.streams.some(s=>s.codec_type==='video'));execFileSync('ffmpeg',['-v','error','-i',path.join(raw,file),'-f','null','-'],{stdio:'pipe'});}
 await host.locator('#refresh-transfers-btn').click();
 await host.waitForFunction(()=>!document.querySelector('#prepare-export-btn').disabled);await host.waitForFunction(()=>document.querySelectorAll('.toast').length===0);await screenshot('05-transfers');
 // Quick delivery [SL-P0-9]: the originals ZIP is downloadable before any export runs.
 const originalsHref=await host.locator('a#download-originals-btn').getAttribute('href');
 const originalsZip=await host.evaluate(async href=>{const r=await fetch(href);return {ok:r.ok,type:r.headers.get('content-type'),bytes:(await r.arrayBuffer()).byteLength};},originalsHref);
 assert.ok(originalsZip.ok&&originalsZip.type==='application/zip'&&originalsZip.bytes>snapshot.tracks.reduce((n,t)=>n+t.totalBytes,0),JSON.stringify(originalsZip));
 const processed=host.waitForResponse(r=>r.url().endsWith('/postprocess')&&r.request().method()==='POST',{timeout:120000});
 await host.locator('#prepare-export-btn').click();
 const processedResponse=await processed;
 assert.ok(processedResponse.ok(),await processedResponse.text());
 const exportStatus=(await (await fetch(`${origin}/api/v1/takes/${snapshot.take.id}`,{headers:{Authorization:`Bearer ${token}`}})).json()).postproc;
 assert.ok(exportStatus?.state==='complete'&&exportStatus.percent===100&&exportStatus.stages.some(s=>s.stage==='video'),JSON.stringify(exportStatus));
 await host.locator('#export-modal').waitFor({state:'visible'});
 await host.waitForFunction(()=>document.querySelectorAll('.toast').length===0);
 await screenshot('06-export');
 // Post-take handoffs (SL-P4-1, SL-P4-3). Built-in is the default engine; without the
 // Mac app it says what to do instead of failing silently.
 const eco=host.locator('#ecosystem-actions');
 await eco.locator('[data-action="transcript"] [data-badge]').filter({hasText:'Unavailable'}).waitFor({timeout:20000});
 assert.match(await eco.locator('[data-action="transcript"] .ecosystem-note').innerText(),/Open p2p.video/);
 assert.equal(await eco.locator('[data-engine]').inputValue(),'builtin');
 assert.equal(await eco.locator('[data-engine] option[value="minutes"]').isDisabled(),false,'the stand-in minutes-cli is detected');
 assert.equal(await eco.locator('[data-run="parachute"]').isDisabled(),true,'no vault is configured in this test');
 const shotEco=async name=>{if(screenshotDir)await eco.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
 await shotEco('06a-export-no-engine');
 // The app connects; the panel notices on its own and enables Transcribe. By default a fake
 // provider; STUDIO_TRANSCRIPTION_PROVIDER=<path to studiolink-captions> runs the real Swift
 // provider with Apple's on-device recognizer instead (opt-in, macOS 26).
 const realProvider=process.env.STUDIO_TRANSCRIPTION_PROVIDER;
 let transcriber,providerProc,providerLog='';
 if(realProvider){
  providerProc=globalThis.__slProvider=spawn(realProvider,['file-provider','--port',String(port),'--data-dir',data],{stdio:['ignore','pipe','pipe']});
  providerProc.stderr.on('data',b=>providerLog+=b);providerProc.stdout.on('data',b=>providerLog+=b);
  transcriber={stats:{cancels:0,jobs:[],errors:[]},close:()=>providerProc.kill('SIGTERM')};
 }else transcriber=await connectFakeTranscriber({origin,dataDir:data,jobMs:4000});
 await eco.locator('[data-run="transcript"]:not([disabled])').waitFor({timeout:30000});
 assert.match(await eco.locator('[data-engine-status]').innerText(),/audio never leaves this Mac/);
 if(!realProvider){
 // First run: cancel while the first track is in flight, then retry.
 await eco.locator('[data-run="transcript"]').click();
 await eco.locator('[data-tracks] [data-track-state="running"]').first().waitFor({timeout:20000});
 await eco.locator('[data-cancel-transcript]').click();
 await host.waitForFunction(()=>document.querySelector('#ecosystem-actions [data-action="transcript"] [data-badge]')?.textContent==='Cancelled',null,{timeout:20000});
 assert.ok(transcriber.stats.cancels>=1,'the provider received the cancel');
 }
 const exportsRoot=path.join(data,'exports'),sessionDir=(await readdir(exportsRoot)).find(name=>!name.startsWith('.'));
 const pkg=path.join(exportsRoot,sessionDir,(await readdir(path.join(exportsRoot,sessionDir))).find(name=>name.startsWith('take_')&&!name.includes('.previous-')));
 assert.equal(await readdir(path.join(pkg,'transcript')).catch(()=>null),null,'a cancelled transcript writes nothing');
 await eco.locator('[data-run="transcript"]:not([disabled])').click();
 if(!realProvider){await eco.locator('[data-tracks] [data-track-state="running"]').first().waitFor({timeout:20000});await shotEco('06b-export-transcribing');}
 await host.waitForFunction(()=>document.querySelector('#ecosystem-actions [data-action="transcript"] [data-badge]')?.textContent==='Done',null,{timeout:120000}).catch(e=>{throw new Error(e.message+' provider log: '+providerLog);});
 await host.waitForFunction(()=>document.querySelector('#ecosystem-actions [data-action="clipEditor"]')?.dataset.state==='success');
 const pkgManifest=JSON.parse(await readFile(path.join(pkg,'manifest.json'),'utf8'));
 const participants=new Set(pkgManifest.sources.map(source=>source.participant));
 let spoken=JSON.parse(await readFile(path.join(pkg,'transcript','transcript.json'),'utf8'));
 assert.equal(spoken.schema,'studiolink.transcript/1');
 assert.equal(spoken.engine.name,'studiolink-builtin');
 assert.equal(spoken.speakers.length,2,'one isolated voice per participant');
 if(realProvider){
  console.log('real built-in transcript:',JSON.stringify(spoken.segments.map(s=>[s.speaker,s.start,s.text])),'engine',JSON.stringify(spoken.engine));
  assert.ok(spoken.speakers.every(s=>s.status==='ok'||s.status==='empty'),JSON.stringify(spoken.speakers));
  if(process.env.STUDIO_FAKE_AUDIO_FILE)assert.ok(spoken.segments.length>0&&spoken.segments.every(s=>participants.has(s.speaker)),'real speech recognized per participant');
 }else{
 assert.equal(spoken.segments.length,2,'one fake segment per track');
 assert.ok(spoken.segments.every(segment=>participants.has(segment.speaker)&&segment.words.length>0),'built-in segments are named after participants, with words');
 assert.ok(transcriber.stats.jobs.every(j=>j.language==='en-US'&&j.durationSec>0),JSON.stringify(transcriber.stats.jobs));
 }
 assert.deepEqual(transcriber.stats.errors,[]);
 for(const file of ['transcript/transcript.srt','transcript/transcript.vtt','transcript/transcript.md','handoff/clip-editor.json','handoff/ave/transcript.json'])await readFile(path.join(pkg,file));
 await shotEco('06c-export-builtin-done');
 // Minutes stays available as an optional engine.
 await eco.locator('[data-engine]').selectOption('minutes');
 await host.waitForFunction(()=>document.querySelector('#ecosystem-actions [data-engine]')?.value==='minutes'&&!document.querySelector('#ecosystem-actions [data-engine]').disabled);
 await eco.locator('[data-run="transcript"]:not([disabled])').click();
 await host.waitForFunction(()=>document.querySelector('#ecosystem-actions [data-action="transcript"] [data-badge]')?.textContent==='Done'&&/Transcript ready/.test(document.querySelector('#ecosystem-actions [data-action="transcript"] .ecosystem-note')?.textContent||''),null,{timeout:60000});
 for(let i=0;i<50;i++){spoken=JSON.parse(await readFile(path.join(pkg,'transcript','transcript.json'),'utf8'));if(spoken.engine.name==='minutes-cli')break;await new Promise(r=>setTimeout(r,100));}
 assert.equal(spoken.engine.name,'minutes-cli');
 assert.equal(spoken.speakers.length,2,'one isolated voice per participant');
 assert.equal(spoken.segments.length,4);
 assert.ok(spoken.segments.every(segment=>participants.has(segment.speaker)&&segment.speaker!=='Ana'),'speakers are participant names, not the CLI label');
 assert.ok(spoken.segments.every((segment,i,all)=>i===0||all[i-1].start<=segment.start),'merged in take order');
 for(const file of ['transcript/transcript.srt','transcript/transcript.vtt','transcript/transcript.md','handoff/clip-editor.json','handoff/program.wav','handoff/ave/transcript.json','handoff/ave/sync.json'])await readFile(path.join(pkg,file));
 // Back to the default for anything that runs after.
 await eco.locator('[data-engine]').selectOption('builtin');
 await host.waitForFunction(()=>document.querySelector('#ecosystem-actions [data-engine]')?.value==='builtin'&&!document.querySelector('#ecosystem-actions [data-engine]').disabled);
 transcriber.close();
 execFileSync('ffmpeg',['-v','error','-i',path.join(pkg,'handoff','program.wav'),'-f','null','-'],{stdio:'pipe'});
 await screenshot('06b-export-handoffs');
 await host.setViewportSize({width:390,height:844});await screenshot('07-export-mobile');
 assert.ok(await host.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth));
 const out=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'out');
 const videos=await readdir(path.join(out,'video')),audios=await readdir(path.join(out,'audio'));
 assert.equal(videos.length,2);assert.equal(audios.length,2,'isolated PCM must replace only the matching embedded audio, without duplicate exports');
 if(pcmEnabled){const manifest=JSON.parse(await readFile(path.join(out,'manifest.json'),'utf8'));const ids=snapshot.tracks.filter(track=>track.kind==='audio').map(track=>track.trackId).sort();assert.deepEqual(manifest.sources.filter(source=>source.kind==='audio').map(source=>source.id).sort(),ids);}
 for(const [directory,files,type] of [['video',videos,'video'],['audio',audios,'audio']])for(const file of files){
  const output=path.join(out,directory,file);
  const info=JSON.parse(execFileSync('ffprobe',['-v','error','-show_streams','-of','json',output],{encoding:'utf8'}));
  assert.ok(info.streams.some(stream=>stream.codec_type===type));
  execFileSync('ffmpeg',['-v','error','-i',output,'-f','null','-'],{stdio:'pipe'});
 }
 assert.deepEqual(errors,[]);console.log(JSON.stringify({participants:2,remoteVideoDecoded:true,assembledTracks:snapshot.tracks.length,decodedFiles:media.length,exportedVideo:videos.length,exportedAudio:audios.length,uncompressedMicrophone:pcmEnabled,errors}));
 }
 }
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 for(const page of [host,guest])if(page&&!page.isClosed())console.error('page diagnostics:',page.url(),(await page.locator('body').innerText({timeout:2000}).catch(()=>'' )).slice(-5000));
 console.error(logs);throw e;
}finally{
 await browser?.close();
 if(globalThis.__slProvider)globalThis.__slProvider.kill('SIGKILL');
 if(daemon.exitCode===null&&daemon.signalCode===null){
  daemon.kill('SIGTERM');
  await Promise.race([new Promise(r=>daemon.once('exit',r)),new Promise(r=>setTimeout(r,3000))]);
  if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');
 }
 await rm(data,{recursive:true,force:true});
}
