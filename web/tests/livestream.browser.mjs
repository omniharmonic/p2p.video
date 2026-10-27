// P3 livestream end-to-end with Chromium fake devices and LOCAL sinks only [SL-P3-1, SL-P3-3].
// Host + one admitted guest; the host composites the call in the browser and
// goes live to two destinations at once:
//   1. rtmps:// to a local TLS-terminating proxy (self-signed certificate,
//      trusted by the daemon only through the test-only --livestream-test-ca
//      flag) in front of an ffmpeg RTMP listener;
//   2. rtmp:// to a second ffmpeg listener, which the test kills mid-stream and
//      restarts: that destination must show "reconnecting", then come back,
//      while destination 1 never notices.
// It also checks that no process on the host shows the stream key in its
// argv (`ps -o args=`), then probes both received FLVs for H.264 + AAC.
// Never publishes to an external service.
//   cd web && STUDIO_BINARY=$PWD/../bin/p2pvideod node tests/livestream.browser.mjs
// Optional: STUDIO_FFMPEG_DIR=<dir with ffmpeg/ffprobe> to run the daemon (and sinks)
// with a specific ffmpeg build, e.g. the LGPL build from scripts/build-ffmpeg-mac.sh.
import assert from 'node:assert/strict';
import {launchURL,readyToJoin} from './lib/harness.mjs';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,stat,mkdir,readFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import net from 'node:net';
import tls from 'node:tls';
import path from 'node:path';
import {revealControl} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const ffDir=process.env.STUDIO_FFMPEG_DIR||'';
const ffmpeg=ffDir?path.join(ffDir,'ffmpeg'):'ffmpeg';
const ffprobe=ffDir&&process.env.STUDIO_FFPROBE!=='system'?path.join(ffDir,'ffprobe'):'ffprobe';
const env={...process.env,PATH:ffDir?`${ffDir}:${process.env.PATH}`:process.env.PATH};
const data=await mkdtemp(path.join(tmpdir(),'studio-livestream-'));
const port=Number(process.env.STUDIO_TEST_PORT||17643),origin=`http://127.0.0.1:${port}`;
const rtmpPort=Number(process.env.STUDIO_RTMP_PORT||19350);
const rtmpPortB=rtmpPort+1;
const streamKey='sl-e2e-secret-key-7f3a';
const flv=path.join(data,'received-rtmps.flv'),flvB1=path.join(data,'received-b1.flv'),flvB2=path.join(data,'received-b2.flv');
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page,fullPage=false)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),fullPage,animations:'disabled'});};
const sleep=ms=>new Promise(r=>setTimeout(r,ms));

// Local RTMP sinks: ffmpeg in listen mode records whatever is published.
const sinks=[];
function startSink(p,out){
 const proc=spawn(ffmpeg,['-hide_banner','-v','error','-listen','1','-i',`rtmp://127.0.0.1:${p}/live/${streamKey}`,'-c','copy','-y',out],{stdio:['ignore','pipe','pipe'],env});
 const s={proc,logs:'',exit:new Promise(r=>proc.once('exit',code=>r(code)))};
 proc.stderr.on('data',b=>s.logs+=b);sinks.push(s);return s;
}
const sinkA=startSink(rtmpPort,flv);
let sinkB=startSink(rtmpPortB,flvB1);

// Local RTMPS ingest: a TLS-terminating proxy with a self-signed certificate
// for 127.0.0.1 in front of sink A. Production trusts only the system store;
// the daemon trusts this certificate only because of --livestream-test-ca.
const certPem=path.join(data,'ingest-cert.pem'),keyPem=path.join(data,'ingest-key.pem');
execFileSync('openssl',['req','-x509','-newkey','rsa:2048','-nodes','-keyout',keyPem,'-out',certPem,'-days','1','-set_serial','1','-subj','/CN=studiolink-test-ingest','-addext','subjectAltName=IP:127.0.0.1'],{stdio:'pipe'});
let tlsHandshakes=0;
const proxy=tls.createServer({key:await readFile(keyPem),cert:await readFile(certPem),minVersion:'TLSv1.2'},sock=>{
 tlsHandshakes++;
 const up=net.connect(rtmpPort,'127.0.0.1');
 sock.pipe(up);up.pipe(sock);
 const end=()=>{sock.destroy();up.destroy();};
 sock.on('error',end);up.on('error',end);sock.on('close',end);up.on('close',end);
});
await new Promise(r=>proxy.listen(0,'127.0.0.1',r));
const tlsPort=proxy.address().port;

const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel','--livestream-test-ca',certPem],{stdio:['ignore','pipe','pipe'],env});
let logs='',browser,host,guest;
const errors=[];
daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await sleep(100);}assert.ok(ready,'daemon did not start: '+logs);
 assert.match(logs,/extra TEST certificate authority/,'the test CA must be announced in the log');
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required']});
 const hostContext=await browser.newContext({permissions:['camera','microphone']});
 const guestContext=await browser.newContext({permissions:['camera','microphone']});
 host=await hostContext.newPage();guest=await guestContext.newPage();
 for(const p of [host,guest]){p.setDefaultTimeout(45000);p.on('pageerror',e=>errors.push(e.message));p.on('console',m=>{if(m.type()==='error')errors.push(`console: ${m.text()}`);});}
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await (await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('Guest');await guest.locator('#join-btn').click();
 await host.locator('#admit-btn').waitFor({state:'visible'});await host.locator('#admit-btn').click();
 await guest.locator('#studio-view').waitFor({state:'visible'});
 // The livestream control is host-only.
 assert.equal(await guest.locator('#livestream-btn').isVisible(),false,'livestream exposed to guest');
 await host.waitForFunction(()=>[...document.querySelectorAll('[id^="remote-"]')].some(v=>v.tagName==='VIDEO'&&v.videoWidth>0),{},{timeout:20000});
 await host.waitForFunction(()=>[...document.querySelectorAll('.tile[data-participant-id] .tile-name')].some(l=>l.textContent==='Guest'));
 console.log('guest admitted, remote video decoded');

 await (await revealControl(host,'#livestream-btn')).click();
 await host.locator('#livestream-panel.open').waitFor({state:'visible'});
 await host.waitForFunction(()=>{const c=document.querySelector('#livestream-preview canvas');if(!c)return false;const d=c.getContext('2d').getImageData(0,0,c.width,c.height).data;let lit=0;for(let i=0;i<d.length;i+=4*97)if(d[i]+d[i+1]+d[i+2]>120)lit++;return lit>50;});
 // Presets default to RTMPS [SL-P3-3].
 assert.equal(await host.locator('#livestream-dest-list [data-field=url]').first().inputValue(),'rtmps://a.rtmps.youtube.com:443/live2');
 for(const [platform,want] of [['twitch','rtmps://ingest.global-contribute.live-video.net/app'],['facebook','rtmps://live-api-s.facebook.com:443/rtmp/'],['linkedin','']]){
  await host.locator('#livestream-dest-list [data-field=platform]').first().selectOption(platform);
  assert.equal(await host.locator('#livestream-dest-list [data-field=url]').first().inputValue(),want,platform);
 }
 // LinkedIn needs RTMPS: a plain rtmp:// URL is refused in the form.
 await host.locator('#livestream-dest-list [data-field=url]').first().fill('rtmp://127.0.0.1/live');
 await host.locator('#livestream-dest-list [data-field=key]').first().fill('x');
 await host.locator('#livestream-go').click();
 assert.match(await host.locator('#livestream-form-error').innerText(),/LinkedIn only accepts rtmps/);

 // Destination 1: RTMPS through the local TLS proxy. Destination 2: plain RTMP.
 await host.locator('#livestream-dest-list [data-field=platform]').first().selectOption('custom');
 await host.locator('#livestream-dest-list [data-field=url]').first().fill(`rtmps://127.0.0.1:${tlsPort}/live`);
 await host.locator('#livestream-dest-list [data-field=key]').first().fill(streamKey);
 assert.equal(await host.locator('#livestream-dest-list [data-field=key]').first().getAttribute('type'),'password');
 await host.locator('#livestream-add-dest').click();
 const second=host.locator('#livestream-dest-list .livestream-dest').nth(1);
 await second.locator('[data-field=platform]').selectOption('custom');
 await second.locator('[data-field=url]').fill(`rtmp://127.0.0.1:${rtmpPortB}/live`);
 await second.locator('[data-field=key]').fill(streamKey);
 await host.locator('#livestream-go').click();
 await host.waitForFunction(()=>document.querySelector('#livestream-status').dataset.phase==='live',{},{timeout:30000});
 const mime=await host.locator('#livestream-status').getAttribute('data-mime');
 console.log('live; MediaRecorder type:',mime);
 const auth={headers:{Authorization:`Bearer ${session.hostToken}`}};
 const status=async()=>{const s=await (await fetch(`${origin}/api/v1/sessions/${session.id}/livestream`,auth)).json();assert.ok(!JSON.stringify(s).includes(streamKey),'status leaked the stream key');return s;};
 const waitStatus=async(what,pred,ms=30000)=>{const end=Date.now()+ms;let s;while(Date.now()<end){s=await status();if(pred(s))return s;await sleep(250);}throw new Error(`timed out waiting for ${what}: ${JSON.stringify(s)}`);};
 await waitStatus('both destinations live',s=>s.destinations.every(d=>d.state==='live'));
 await screenshot('livestream-live',host);

 // No process on this Mac shows the key: not the daemon, not its ffmpeg [SL-P3-3].
 const children=execFileSync('pgrep',['-P',String(daemon.pid)],{encoding:'utf8'}).trim().split('\n').filter(Boolean);
 const argvs=[daemon.pid,...children].map(pid=>({pid,args:execFileSync('ps',['-o','args=','-p',String(pid)],{encoding:'utf8'}).trim()}));
 assert.ok(argvs.some(p=>p.args.includes('h264_videotoolbox')&&p.args.includes('pipe:1')),'encoder process not found: '+JSON.stringify(argvs));
 for(const p of argvs){assert.ok(!p.args.includes(streamKey),`pid ${p.pid} shows the stream key in ps`);assert.ok(!/rtmps?:\/\//.test(p.args),`pid ${p.pid} shows a destination in ps: ${p.args}`);}
 // The test's own sinks listen on .../live/<key>, so they are the only exception.
 const sinkPids=new Set(sinks.map(s=>String(s.proc.pid)));
 const leaks=execFileSync('ps',['-axww','-o','pid=,args='],{encoding:'utf8'}).split('\n').map(l=>l.trim()).filter(l=>l.includes(streamKey)&&!sinkPids.has(l.split(/\s+/)[0]));
 assert.deepEqual(leaks,[],'a process on this host shows the stream key');
 console.log('ps: key absent from',argvs.length,'daemon processes and the full process list');

 await sleep(6000);
 const live=await status();
 assert.equal(live.state,'live',JSON.stringify(live));
 assert.ok(live.bytesIn>100_000,JSON.stringify(live));
 assert.ok(live.progress.frames>=150&&live.progress.fps>20,JSON.stringify(live.progress));
 assert.ok(live.ingestKbps>100,JSON.stringify(live));
 // The daemon measures the encoder's output itself now [SL-P3-2, SL-P3-3].
 assert.ok(live.bitrateKbps>100&&live.bitrateSource==='output',JSON.stringify(live));
 assert.match(await host.locator('#livestream-status [data-bitrate]').innerText(),/\d+(\.\d)? (kbps|Mbps)/);
 assert.ok(live.progress.speed>0.8&&live.progress.speed<1.3,'encoder not keeping realtime pace: '+JSON.stringify(live.progress));
 assert.equal(live.destinations[0].server,`rtmps://127.0.0.1:${tlsPort}`);
 assert.equal(live.destinations[1].server,`rtmp://127.0.0.1:${rtmpPortB}`);
 for(const d of live.destinations)assert.ok(d.state==='live'&&d.kbps>100&&d.reconnects===0,JSON.stringify(d));
 assert.ok(tlsHandshakes>=1,'rtmps destination never reached the TLS proxy');
 assert.deepEqual(live.warnings,[],JSON.stringify(live.warnings));

 // Kill destination 2's sink mid-stream: it alone goes to "reconnecting".
 sinkB.proc.kill('SIGKILL');await sinkB.exit;
 const dropped=await waitStatus('destination 2 reconnecting',s=>s.destinations[1].state==='reconnecting',15000);
 assert.equal(dropped.destinations[0].state,'live','destination 1 disturbed by destination 2 dropping');
 assert.ok(dropped.destinations[1].error&&dropped.destinations[1].attempt>=1,JSON.stringify(dropped.destinations[1]));
 assert.ok(dropped.warnings.some(w=>w.includes('reconnecting')),JSON.stringify(dropped.warnings));
 await host.waitForFunction(()=>document.querySelector('#livestream-status li[data-index="1"]')?.dataset.state==='reconnecting',{},{timeout:10000});
 assert.match(await host.locator('#livestream-status li[data-index="1"] [data-dest-line]').innerText(),/Reconnecting · attempt \d+ of \d+/);
 assert.ok(await host.locator('#livestream-warnings li').count()>=1,'no health warning shown in the panel');
 await screenshot('livestream-reconnecting',host);
 console.log('destination 2 reconnecting:',dropped.destinations[1].error);
 await sleep(2000);
 sinkB=startSink(rtmpPortB,flvB2);
 const back=await waitStatus('destination 2 live again',s=>s.destinations[1].state==='live'&&s.destinations[1].reconnects>0,40000);
 assert.equal(back.destinations[0].reconnects,0,'destination 1 reconnected');
 await host.waitForFunction(()=>document.querySelector('#livestream-status li[data-index="1"]')?.dataset.state==='live',{},{timeout:10000});
 console.log('destination 2 back after',back.destinations[1].reconnects,'retries');
 await sleep(5000);

 await host.locator('#livestream-end').click();
 await host.waitForFunction(()=>document.querySelector('#livestream-status').dataset.phase==='stopped',{},{timeout:15000});
 for(const [name,s] of [['rtmps sink',sinkA],['restarted sink',sinkB]]){
  const code=await Promise.race([s.exit,sleep(15000).then(()=>'timeout')]);
  assert.notEqual(code,'timeout',`${name} did not finish after the stream ended: ${s.logs}`);
 }
 const probe=f=>JSON.parse(execFileSync(ffprobe,['-v','error','-show_streams','-show_format','-of','json',f],{encoding:'utf8'}));
 const check=(f,minSec)=>{
  const info=probe(f);
  const video=info.streams.find(s=>s.codec_type==='video'),audio=info.streams.find(s=>s.codec_type==='audio');
  assert.equal(video?.codec_name,'h264',JSON.stringify(info.streams));
  assert.equal(audio?.codec_name,'aac',JSON.stringify(info.streams));
  assert.equal(Number(audio.sample_rate),48000);
  assert.equal(video.width,1280);assert.equal(video.height,720);
  const duration=Number(info.format.duration);
  assert.ok(duration>=minSec,`${path.basename(f)}: received only ${duration}s`);
  execFileSync(ffmpeg,['-v','error','-i',f,'-f','null','-'],{stdio:'pipe',env});
  return {video:`${video.codec_name} ${video.width}x${video.height}`,audio:`${audio.codec_name} ${audio.sample_rate}Hz`,durationSec:duration};
 };
 assert.ok((await stat(flv)).size>100_000);
 const rtmps=check(flv,15),restarted=check(flvB2,3);
 assert.ok(restarted.durationSec<rtmps.durationSec,'restarted sink cannot have more than the uninterrupted one');
 if(process.env.STUDIO_KEEP_FLV)execFileSync('cp',[flv,process.env.STUDIO_KEEP_FLV]);
 console.log('progress at 8s:',JSON.stringify(live.progress));
 assert.ok(!logs.includes(streamKey),'daemon logs contain the stream key');
 assert.match(logs,/livestream destination reconnecting/);
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({livestream:'ok',mimeType:mime,bytesIn:live.bytesIn,fps:live.progress.fps,bitrateKbps:live.bitrateKbps,bitrateSource:live.bitrateSource,tlsHandshakes,reconnects:back.destinations[1].reconnects,received:{rtmps,restarted},errors}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 for(const page of [host,guest])if(page&&!page.isClosed())console.error('page diagnostics:',(await page.locator('body').innerText({timeout:2000}).catch(()=>'')).slice(-3000));
 console.error('daemon logs:',logs.replaceAll(streamKey,'<key>'));for(const s of sinks)console.error('sink logs:',s.logs);
 throw e;
}finally{
 await browser?.close();
 proxy.close();
 for(const proc of [daemon,...sinks.map(s=>s.proc)])if(proc.exitCode===null&&proc.signalCode===null){
  proc.kill('SIGTERM');
  await Promise.race([new Promise(r=>proc.once('exit',r)),sleep(3000)]);
  if(proc.exitCode===null&&proc.signalCode===null)proc.kill('SIGKILL');
 }
 await rm(data,{recursive:true,force:true});
}
