// Live captions [SL-P4-2] end to end against the real daemon with Chromium
// fake devices fed from WAV files: the host's mic is a steady 600 Hz tone, the
// guest's mic is synthesized speech. A fake caption provider (the Mac app's
// protocol, tests/lib/fake-caption-provider.mjs) classifies each stream as
// "Tone" or "Speech", so the test proves per-participant routing: the guest's
// audio crosses WebRTC to the host's page, is tapped there, reaches the
// provider as its own stream and comes back attributed to the guest.
// Verifies: host turns captions on with `c`; "Captions on" indicator for
// everyone and the waiting room; captions render on both pages with correct
// speaker names ("You" for yourself); per-viewer hide/show with `c` and the CC
// button, remembered; guests can't turn captions on; VTT/JSON transcript offered
// to the host at meeting end; no caption text or tokens in the daemon log.
// Build first; STUDIO_BINARY points at the daemon.
import assert from 'node:assert/strict';
import {spawn, execFileSync} from 'node:child_process';
import {mkdtemp,rm,readFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,launchURL,readyToJoin} from './lib/harness.mjs';
import {connectFakeProvider} from './lib/fake-caption-provider.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-captions-'));
const port=Number(process.env.STUDIO_TEST_PORT||18643),origin=`http://127.0.0.1:${port}`;
const ffmpeg=process.env.FFMPEG||'ffmpeg';
const wait=ms=>new Promise(r=>setTimeout(r,ms));

// Test audio: a tone for the host, speech for the guest (macOS `say`, or a
// speech-like modulated noise elsewhere).
const toneWav=path.join(data,'host-tone.wav'),speechWav=path.join(data,'guest-speech.wav');
execFileSync(ffmpeg,['-loglevel','error','-y','-f','lavfi','-i','sine=frequency=600:sample_rate=48000:duration=20','-af','volume=0.5','-ac','1','-c:a','pcm_s16le',toneWav]);
try{
 const aiff=path.join(data,'speech.aiff');
 execFileSync('say',['-v','Samantha','-o',aiff,'Welcome to the p2p.video podcast. Today we are testing private live captions. Every participant gets their own caption stream, recognized on the host computer.']);
 execFileSync(ffmpeg,['-loglevel','error','-y','-i',aiff,'-ar','48000','-ac','1','-c:a','pcm_s16le',speechWav]);
}catch{
 execFileSync(ffmpeg,['-loglevel','error','-y','-f','lavfi','-i','anoisesrc=color=pink:sample_rate=48000:duration=20,volume=0.4','-af','tremolo=f=4:d=0.9','-ac','1','-c:a','pcm_s16le',speechWav]);
}

const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const errors=[];const browsers=[];let provider;
const launch=wav=>chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream',`--use-file-for-fake-audio-capture=${wav}`,'--autoplay-policy=no-user-gesture-required','--mute-audio']}).then(b=>{browsers.push(b);return b;});
const open=async(browser,name)=>{
 const context=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:800},acceptDownloads:true});
 const page=await context.newPage();page.setDefaultTimeout(30000);
 page.on('console',m=>{if(m.type()==='error'&&!(m.text().includes('422')&&m.location().url.endsWith('/invite')))errors.push(`${name} console: ${m.text()}`);});
 page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
 return page;
};
const lines=page=>page.$$eval('#captions-overlay .caption-line',rows=>{let who='';return rows.map(r=>{const s=r.querySelector('.caption-speaker');if(s)who=s.textContent;return `${who}: ${r.querySelector('.caption-text').textContent}`;});});
const pressC=async page=>{await page.locator('body').click({position:{x:5,y:5}});await page.keyboard.press('KeyC');};

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 provider=await connectFakeProvider({origin,dataDir:data});
 console.log('fake caption provider connected');

 const host=await open(await launch(toneWav),'Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 const guestBrowser=await launch(speechWav);
 const guest=await open(guestBrowser,'Guest');
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('Guest');await guest.locator('#join-btn').click();
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex');
 await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
 await host.waitForFunction(()=>[...document.querySelectorAll('#video-grid .tile[data-participant-id] video')].some(v=>v.srcObject?.getAudioTracks().length));
 console.log('host + guest in call');

 // Controls before captions: CC for both, the settings menu for the host only.
 await host.locator('#cc-btn').waitFor();await guest.locator('#cc-btn').waitFor();
 assert.equal(await guest.locator('#cc-menu-btn').isHidden(),true,'guests have no caption settings');
 assert.equal(await host.locator('#captions-indicator').isHidden(),true);
 await host.locator('#cc-menu-btn').click();
 await host.locator('#cc-provider-status',{hasText:/Ready/}).waitFor();
 await host.keyboard.press('Escape');
 // A guest can't turn captions on (UI says so; the hub refuses it anyway).
 await pressC(guest);
 await guest.locator('.toast',{hasText:'hasn’t turned on captions'}).waitFor();
 console.log('captions off: CC visible, guest cannot enable, host sees the recognizer ready');

 // A waiting guest is told captions are on before admission.
 const waiting=await open(guestBrowser,'Waiting');
 await waiting.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await waiting.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await waiting.locator('#name-input').fill('Waiting');await waiting.locator('#join-btn').click();
 await waiting.locator('#waiting-room').waitFor({state:'visible'});

 // Host turns captions on with `c`.
 await pressC(host);
 for(const page of [host,guest])await page.locator('#captions-indicator',{hasText:'Captions on'}).waitFor();
 await waiting.locator('#waiting-captions-notice').waitFor({state:'visible'});
 assert.equal(await host.locator('#cc-btn').getAttribute('aria-pressed'),'true');
 console.log('captions on: indicator for everyone, notice in the waiting room');

 // Captions arrive on both pages with the right speaker names and content.
 const until=async(page,re,label)=>{
  const deadline=Date.now()+30000;
  for(;;){const l=await lines(page);if(l.some(x=>re.test(x)))return l;if(Date.now()>deadline)throw new Error(`${label}: no caption matching ${re}; saw ${JSON.stringify(l)}`);await wait(200);}
 };
 await until(host,/^Guest: Speech heard/,'host sees guest');
 await until(host,/^You: Tone heard/,'host sees self');
 await until(guest,/^Host: Tone heard/,'guest sees host');
 await until(guest,/^You: Speech heard/,'guest sees self');
 assert.ok(provider.stats.starts>=2&&provider.stats.frames>=20,`provider got per-participant streams: ${JSON.stringify(provider.stats)}`);
 assert.ok(provider.stats.languages.every(l=>l==='en-US'),'default language en-US');
 assert.equal(await waiting.locator('#captions-overlay .caption-line').count(),0,'the waiting room never receives captions');
 console.log('captions rendered on both pages with correct speakers (tone→Host, speech→Guest)');

 // Partials are replaced by finals in place (never more than 3 lines).
 await until(host,/^Guest: Speech heard on this stream\./,'guest final');
 assert.ok((await lines(host)).length<=3,'at most three caption lines');

 // Per-viewer toggle with `c` and the CC button; remembered locally.
 await pressC(guest);
 await guest.waitForFunction(()=>document.querySelector('#captions-overlay').hidden);
 assert.equal(await guest.evaluate(()=>localStorage.getItem('studiolink.captions.visible')),'0');
 assert.equal(await host.locator('#captions-overlay').isHidden(),false,'hiding is per viewer');
 assert.equal(await guest.locator('#captions-indicator').isVisible(),true,'the privacy indicator stays while captions are hidden');
 await guest.locator('#cc-btn').click();
 await guest.waitForFunction(()=>!document.querySelector('#captions-overlay').hidden);
 assert.equal(await guest.evaluate(()=>localStorage.getItem('studiolink.captions.visible')),'1');
 console.log('per-viewer hide/show with c and CC, remembered');

 // Host switches captions off and on again from the menu.
 await host.locator('#cc-menu-btn').click();
 await host.locator('#cc-room-switch').click();
 await guest.locator('#captions-indicator').waitFor({state:'hidden'});
 await waiting.locator('#waiting-captions-notice').waitFor({state:'hidden'});
 await host.locator('#cc-room-switch').click();
 await guest.locator('#captions-indicator').waitFor({state:'visible'});
 await host.keyboard.press('Escape');
 await until(guest,/^Host: Tone heard/,'captions resume');
 console.log('host turned captions off and on for the room');

 // Meeting end: the host is offered the transcript.
 await host.locator('#end-meeting-btn').click();
 await host.locator('.room-dialog [data-confirm]').click();
 await host.locator('#captions-transcript-dialog').waitFor({state:'visible'});
 const [download]=await Promise.all([host.waitForEvent('download'),host.locator('#captions-transcript-dialog [data-format="vtt"]').click()]);
 const vtt=await readFile(await download.path(),'utf8');
 assert.match(vtt,/^WEBVTT/);
 assert.match(vtt,/\d\d:\d\d:\d\d\.\d{3} --> \d\d:\d\d:\d\d\.\d{3}\n<v Guest>Speech heard on this stream\./);
 assert.match(vtt,/<v Host>Tone heard on this stream\./);
 assert.equal(download.suggestedFilename(),`captions-${session.id}.vtt`);
 const [jsonDl]=await Promise.all([host.waitForEvent('download'),host.locator('#captions-transcript-dialog [data-format="json"]').click()]);
 const transcript=JSON.parse(await readFile(await jsonDl.path(),'utf8'));
 assert.equal(transcript.source,'live-captions');
 assert.ok(transcript.segments.some(s=>s.name==='Guest'&&s.text.startsWith('Speech')));
 assert.equal(await guest.locator('#captions-transcript-dialog').isVisible(),false,'only the host is offered the transcript');
 console.log(`transcript offered at meeting end (${transcript.segments.length} final segments)`);

 await wait(500);
 assert.ok(!/heard on this stream|Speech heard|Tone heard/.test(logs),'caption text leaked into the daemon log');
 assert.ok(!logs.includes(session.hostToken)&&!logs.includes(session.token),'token leaked into the daemon log');
 assert.ok(!logs.includes((await readFile(path.join(data,'.desktop_token'),'utf8')).trim()),'desktop token leaked into the daemon log');
 assert.deepEqual(errors,[]);
 console.log('PASS captions browser test');
}catch(e){
 console.error(e);console.error('--- daemon log tail ---\n'+logs.split('\n').slice(-40).join('\n'));
 if(errors.length)console.error('page errors:',errors);
 process.exitCode=1;
}finally{
 provider?.close();
 for(const b of browsers)await b.close().catch(()=>{});
 daemon.kill('SIGTERM');await wait(300);
 await rm(data,{recursive:true,force:true});
}
