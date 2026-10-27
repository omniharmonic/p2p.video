// Branding and lower thirds [SL-P5-2] against the real daemon with Chromium
// fake devices and a LOCAL RTMP sink only. The host uploads a logo, sets the
// show look and the guest's lower third; tiles show name + title for both;
// the guest can't change any of it; the livestream goes out unbranded, then
// branded with an on-demand lower third, and frames extracted with ffmpeg
// prove the logo is drawn only in the branded part; a take's manifest.json
// carries the titles (the recording itself stays clean).
//   make build && cd web && STUDIO_TEST_PORT=19152 STUDIO_BINARY=$PWD/../bin/p2pvideod \
//     STUDIO_FFMPEG_DIR=<dir with ffmpeg/ffprobe> node tests/branding.browser.mjs
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,mkdir,readFile,rm,stat,writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';
import {isolatedNetworkArgs,launchURL,revealControl,startTake,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const ffDir=process.env.STUDIO_FFMPEG_DIR||'';
const ffmpeg=ffDir?path.join(ffDir,'ffmpeg'):'ffmpeg';
const ffprobe=ffDir?path.join(ffDir,'ffprobe'):'ffprobe';
const env={...process.env,PATH:ffDir?`${ffDir}:${process.env.PATH}`:process.env.PATH};
const data=await mkdtemp(path.join(tmpdir(),'studio-branding-'));
const port=Number(process.env.STUDIO_TEST_PORT||19152),origin=`http://127.0.0.1:${port}`;
const rtmpPort=Number(process.env.STUDIO_RTMP_PORT||port+200);
const streamKey='sl-brand-e2e-key-91c2';
const flv=path.join(data,'received.flv');
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const errors=[];let browser;

/** A solid-colour RGB PNG, written by hand (zlib + CRC). */
function solidPNG(w,h,[r,g,b]){
 const chunk=(type,body)=>{const len=Buffer.alloc(4);len.writeUInt32BE(body.length);const tb=Buffer.concat([Buffer.from(type),body]);const crc=Buffer.alloc(4);crc.writeUInt32BE(zlib.crc32(tb)>>>0);return Buffer.concat([len,tb,crc]);};
 const ihdr=Buffer.alloc(13);ihdr.writeUInt32BE(w,0);ihdr.writeUInt32BE(h,4);ihdr[8]=8;ihdr[9]=2;
 const row=Buffer.alloc(1+w*3);for(let x=0;x<w;x++){row[1+x*3]=r;row[2+x*3]=g;row[3+x*3]=b;}
 return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n','binary'),chunk('IHDR',ihdr),chunk('IDAT',zlib.deflateSync(Buffer.concat(Array(h).fill(row)))),chunk('IEND',Buffer.alloc(0))]);
}
const region=(t,{x,y,w,h})=>execFileSync(ffmpeg,['-v','error','-ss',String(t),'-i',flv,'-frames:v','1','-vf',`crop=${w}:${h}:${x}:${y}`,'-f','rawvideo','-pix_fmt','rgb24','-'],{env,maxBuffer:1<<24});
/** Mean RGB of a region of the frame at t seconds. */
function regionMean(t,rect){
 const raw=region(t,rect);
 const sum=[0,0,0];for(let i=0;i<raw.length;i+=3){sum[0]+=raw[i];sum[1]+=raw[i+1];sum[2]+=raw[i+2];}
 const n=raw.length/3;return sum.map(v=>Math.round(v/n));
}
/** Share of near-white pixels (text) in a region. */
function brightShare(t,rect){
 const raw=region(t,rect);let n=0;for(let i=0;i<raw.length;i+=3)if(raw[i]>200&&raw[i+1]>200&&raw[i+2]>200)n++;return n/(raw.length/3);
}
const captureSockets=()=>{const W=window.WebSocket;window.__sockets=[];window.WebSocket=class extends W{constructor(...a){super(...a);window.__sockets.push(this);}};};

const sink=spawn(ffmpeg,['-hide_banner','-v','error','-listen','1','-i',`rtmp://127.0.0.1:${rtmpPort}/live/${streamKey}`,'-c','copy','-y',flv],{stdio:['ignore','pipe','pipe'],env});
let sinkLogs='';sink.stderr.on('data',b=>sinkLogs+=b);
const sinkExit=new Promise(r=>sink.once('exit',code=>r(code)));
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe'],env});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async name=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:800}});
  await context.addInitScript(captureSockets);
  const page=await context.newPage();page.setDefaultTimeout(45000);
  page.on('console',m=>{if(m.type()==='error'&&!/Failed to load resource/.test(m.text()))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  return page;
 };
 const host=await open('Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Ben');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 const guest=await open('Guest');
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('ana');await guest.locator('#join-btn').click();
 await host.locator('#admit-btn').waitFor({state:'visible'});await host.locator('#admit-btn').click();
 await guest.locator('#studio-view').waitFor({state:'visible'});
 await host.waitForFunction(()=>[...document.querySelectorAll('#video-grid .tile[data-participant-id] video')].some(v=>v.videoWidth>0));
 const guestId=await host.evaluate(()=>document.querySelector('#video-grid .tile[data-participant-id]').dataset.participantId);
 console.log('guest admitted',guestId);

 // Guests have no branding controls, and the hub/HTTP refuse them.
 assert.equal(await guest.locator('#branding-btn').isHidden(),true,'guest sees Branding');
 const putAsGuest=await fetch(`${origin}/api/v1/sessions/${session.id}/branding`,{method:'PUT',headers:{Authorization:`Bearer ${session.token}`,'Content-Type':'application/json'},body:'{"accent":"#000000"}'});
 assert.ok([401,403].includes(putAsGuest.status),'guest invite changed branding: '+putAsGuest.status);

 // Host: show look + logo.
 await (await revealControl(host,'#branding-btn')).click();
 await host.locator('#branding-panel.open').waitFor({state:'visible'});
 assert.equal(await host.locator('#brand-remember-hint').isHidden(),true,'an instant meeting claims to remember titles');
 await host.locator('#brand-show-name').fill('The Long Take');
 await host.locator('#brand-accent').fill('#e07a5f');
 const logoPath=path.join(data,'logo.png');await writeFile(logoPath,solidPNG(400,200,[255,0,255]));
 const svgPath=path.join(data,'logo.svg');await writeFile(svgPath,'<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>');
 await host.locator('#brand-logo-input').setInputFiles(svgPath);
 await host.waitForFunction(()=>/PNG or JPEG/.test(document.querySelector('#brand-status').textContent));
 await host.locator('#brand-logo-input').setInputFiles(logoPath);
 await host.waitForFunction(()=>document.querySelector('#brand-status').textContent==='Logo saved.');
 await host.waitForFunction(()=>document.querySelector('#brand-logo-preview img')?.naturalWidth===400);
 assert.match(await host.locator('#brand-logo-preview img').getAttribute('src'),/^blob:/,'logo preview is not a same-origin blob');
 // The served logo has the right headers.
 const served=await fetch(`${origin}/api/v1/sessions/${session.id}/branding/logo`,{headers:{Authorization:`Bearer ${session.hostToken}`}});
 assert.equal(served.headers.get('content-type'),'image/png');assert.equal(served.headers.get('x-content-type-options'),'nosniff');
 assert.equal((await fetch(`${origin}/api/v1/sessions/${session.id}/branding/logo`)).status,401,'logo served without a host token');
 await host.locator('#brand-tile-labels').click();
 await host.locator('#brand-lt-mode').selectOption('manual');
 await host.locator('#brand-save').click();
 await host.waitForFunction(()=>/^Saved/.test(document.querySelector('#brand-status').textContent));

 // Lower thirds: the guest's name and title.
 const row=host.locator(`[data-lt-row="${guestId}"]`);
 await row.waitFor({state:'visible'});
 await row.locator('[data-field=name]').fill('Ana Ruiz');
 await row.locator('[data-field=title]').fill('Founder, X');
 await row.locator('[data-save]').click();
 const hostRow=host.locator('form.branding-person').first();
 await hostRow.locator('[data-field=title]').fill('Host, The Long Take');
 await hostRow.locator('[data-save]').click();
 await host.waitForFunction(id=>document.querySelector(`#video-grid .tile[data-participant-id="${id}"] .tile-lower-third .lt-title`)?.textContent==='Founder, X',guestId);
 await screenshot('p5-branding-settings',host);
 await host.locator('#branding-panel [data-close]').click();
 assert.equal(await host.locator(`#video-grid .tile[data-participant-id="${guestId}"] .lt-name`).textContent(),'Ana Ruiz');
 assert.equal(await host.locator('#local-tile .lt-title').textContent(),'Host, The Long Take');
 // The guest sees the same labels, its own tile included.
 await guest.waitForFunction(()=>document.querySelector('#local-tile .tile-lower-third .lt-name')?.textContent==='Ana Ruiz'&&document.querySelector('#local-tile .lt-title').textContent==='Founder, X');
 await guest.waitForFunction(()=>/Host, The Long Take/.test(document.querySelector('#video-grid .tile[data-participant-id] .tile-lower-third')?.textContent||''));
 await screenshot('p5-tile-lower-thirds',guest);
 // Negative: a forged lower third from the guest's socket is refused.
 await guest.evaluate(id=>window.__sockets.filter(s=>s.readyState===1).at(-1).send(JSON.stringify({t:'lower_third',payload:{participantId:id,title:'CEO of Everything'}})),guestId);
 await guest.waitForFunction(()=>/Only the host or a producer/.test(document.querySelector('#toast-container').textContent));
 await wait(400);
 assert.equal(await host.locator(`#video-grid .tile[data-participant-id="${guestId}"] .lt-title`).textContent(),'Founder, X','guest changed its title');
 console.log('tile lower thirds ok; guest refused');

 // Livestream: unbranded first, then branded with an on-demand lower third.
 await (await revealControl(host,'#livestream-btn')).click();
 await host.locator('#livestream-panel.open').waitFor({state:'visible'});
 await host.locator('#livestream-dest-list [data-field=platform]').first().selectOption('custom');
 await host.locator('#livestream-dest-list [data-field=url]').first().fill(`rtmp://127.0.0.1:${rtmpPort}/live`);
 await host.locator('#livestream-dest-list [data-field=key]').first().fill(streamKey);
 await host.locator('#livestream-go').click();
 await host.waitForFunction(()=>document.querySelector('#livestream-status').dataset.phase==='live',{},{timeout:30000});
 await wait(5000);
 await host.locator('#livestream-branded').click();
 await host.locator(`#livestream-lt-cues [data-cue="${guestId}"]`).click();
 // The preview canvas draws the logo top right (magenta) once branded.
 await host.waitForFunction(()=>{const c=document.querySelector('#livestream-preview canvas');const d=c.getContext('2d').getImageData(c.width-60,30,8,8).data;return d[0]>200&&d[1]<80&&d[2]>200;},{},{timeout:10000});
 await wait(1200);
 await screenshot('p5-livestream-panel',host);
 await wait(4500);
 await host.locator('#livestream-end').click();
 await host.waitForFunction(()=>document.querySelector('#livestream-status').dataset.phase==='stopped',{},{timeout:15000});
 const code=await Promise.race([sinkExit,wait(15000).then(()=>'timeout')]);
 assert.notEqual(code,'timeout','RTMP sink did not finish: '+sinkLogs);
 assert.ok((await stat(flv)).size>100_000);
 const duration=Number(JSON.parse(execFileSync(ffprobe,['-v','error','-show_format','-of','json',flv],{encoding:'utf8',env})).format.duration);
 assert.ok(duration>=9,`received only ${duration}s`);
 // Logo rect for a 400×200 logo in the 1280×720 branded band (lib/branding.ts logoRect).
 const logo={x:1170,y:13,w:92,h:46};
 const inner={x:logo.x+10,y:logo.y+8,w:logo.w-20,h:logo.h-16};
 const before=regionMean(2,inner),after=regionMean(duration-2.5,inner);
 console.log('logo region mean rgb',JSON.stringify({unbranded:before,branded:after}));
 assert.ok(after[0]>200&&after[1]<80&&after[2]>200,'logo missing from the branded stream: '+after);
 assert.ok(!(before[0]>200&&before[1]<80&&before[2]>200),'logo present before branding: '+before);
 // The branded band uses the accent; the unbranded frame is plain graphite there.
 const band={x:400,y:20,w:200,h:30};
 const bandBefore=regionMean(2,band),bandAfter=regionMean(duration-2.5,band);
 assert.ok(bandAfter[0]-bandBefore[0]>25,'branded background missing: '+JSON.stringify({bandBefore,bandAfter}));
 // The show name is drawn in the band, and the guest's lower third uses the accent (#e07a5f).
 const nameText={x:60,y:16,w:260,h:40};
 assert.ok(brightShare(duration-2.5,nameText)>0.05&&brightShare(2,nameText)<0.01,'show name missing from the branded band');
 const strip=regionMean(duration-2.5,{x:672,y:560,w:6,h:80});
 assert.ok(Math.abs(strip[0]-224)<30&&Math.abs(strip[1]-122)<30&&Math.abs(strip[2]-95)<30,'lower-third accent strip: '+strip);
 assert.ok(brightShare(duration-2.5,{x:700,y:560,w:160,h:40})>0.05,'lower-third name text missing');
 if(screenshotDir)execFileSync(ffmpeg,['-v','error','-ss',String(duration-2.5),'-i',flv,'-frames:v','1','-y',path.join(screenshotDir,'p5-livestream-lower-third.png')],{env});
 console.log('livestream branding verified from the RTMP output');

 // A take's export carries the lower thirds; recordings stay clean.
 await host.locator('#livestream-close-btn').click();
 await startTake(host);
 await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await wait(4000);
 await host.locator('#rec-btn').click();
 let snapshot;
 for(let i=0;i<120;i++){
  const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
  const take=detail.takes?.[0];
  if(take){snapshot=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();if(snapshot.tracks.length>=2&&snapshot.tracks.every(t=>t.status==='assembled')&&!snapshot.missingParticipants.length)break;}
  await wait(500);
 }
 const post=await fetch(`${origin}/api/v1/takes/${snapshot.take.id}/postprocess`,{method:'POST',headers:{Authorization:`Bearer ${session.hostToken}`}});
 assert.ok(post.ok,await post.text());
 const manifest=JSON.parse(await readFile(path.join(data,'sessions',session.id,'takes',snapshot.take.id,'out','manifest.json'),'utf8'));
 const byId=Object.fromEntries((manifest.participants||[]).map(p=>[p.id,p]));
 assert.deepEqual({name:byId[guestId]?.name,lowerThirdName:byId[guestId]?.lowerThirdName,title:byId[guestId]?.title},{name:'ana',lowerThirdName:'Ana Ruiz',title:'Founder, X'},JSON.stringify(manifest.participants));
 assert.ok(Object.values(byId).some(p=>p.role==='host'&&p.title==='Host, The Long Take'),JSON.stringify(manifest.participants));
 console.log('manifest participants:',JSON.stringify(manifest.participants));

 assert.ok(!logs.includes(streamKey),'daemon logs contain the stream key');
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({branding:'ok',logo:{unbranded:before,branded:after},durationSec:duration}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 console.error('daemon logs:',logs.replaceAll(streamKey,'<key>').slice(-5000));console.error('sink logs:',sinkLogs);
 throw e;
}finally{
 await browser?.close();
 for(const proc of [daemon,sink])if(proc.exitCode===null&&proc.signalCode===null){
  proc.kill('SIGTERM');
  await Promise.race([new Promise(r=>proc.once('exit',r)),wait(3000)]);
  if(proc.exitCode===null&&proc.signalCode===null)proc.kill('SIGKILL');
 }
 await rm(data,{recursive:true,force:true});
}
