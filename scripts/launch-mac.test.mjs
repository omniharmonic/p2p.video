// Temporary bundle/profile only. Tests never stop a daemon they did not start.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {createServer,get as httpGet} from 'node:http';
import {mkdtemp,mkdir,copyFile,writeFile,readFile,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
const root=await mkdtemp(path.join(tmpdir(),'studio-launcher-'));
const resource=path.join(root,'p2p.video.app/Contents/Resources'),macos=path.join(root,'p2p.video.app/Contents/MacOS');
let ownedPid;
const buildId=process.env.STUDIO_BUILD_ID||'development';
const binary=process.env.STUDIO_BINARY||'/tmp/studiolink-audit-daemon';
const shellQuote=value=>"'"+value.replaceAll("'","'\\''")+"'";
const serverListen=async(server)=>{await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));return server.address().port;};
const run=async(port,extraEnv={})=>{
 const child=spawn('zsh',[path.join(macos,'p2p.video')],{env:{...process.env,PATH:path.join(root,'commands')+':'+process.env.PATH,STUDIOLINK_PORT:String(port),STUDIOLINK_DATA_DIR:path.join(root,'data'),STUDIOLINK_NO_OPEN:'1',...extraEnv},stdio:['ignore','pipe','pipe']});
 let output='';child.stdout.on('data',b=>output+=b);child.stderr.on('data',b=>output+=b);
 const timer=setTimeout(()=>child.kill('SIGKILL'),15000);
 const code=await new Promise(resolve=>child.once('exit',resolve));clearTimeout(timer);return {code,output};
};
try{
 await mkdir(path.join(resource,'bin'),{recursive:true});await mkdir(macos,{recursive:true});await mkdir(path.join(root,'commands'));
 await copyFile(new URL('./launch-mac.sh',import.meta.url),path.join(macos,'p2p.video'));
 await writeFile(path.join(resource,'build-id.txt'),buildId+'\n');
 await writeFile(path.join(resource,'bin/ffmpeg'),'#!/bin/sh\nexit 0\n',{mode:0o700});
 await writeFile(path.join(root,'commands/osascript'),'#!/bin/sh\nexit 0\n',{mode:0o700});
 await writeFile(path.join(resource,'bin/p2pvideod'),`#!/bin/zsh\nprint -r -- $$ > ${shellQuote(path.join(root,'owned.pid'))}\n[[ $(command -v ffmpeg) == ${shellQuote(path.join(resource,'bin/ffmpeg'))} ]] || exit 97\nexec ${shellQuote(binary)} "$@"\n`,{mode:0o700});
 for(const health of [{status:'ok'},{status:'ok',product:'unrelated',buildId},{status:'ok',product:'com.omniharmonic.studiolink',buildId:'older-build'}]){
  const server=createServer((_req,res)=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify(health));});const port=await serverListen(server);
  try{const result=await run(port);assert.equal(result.code,1,result.output);assert.match(result.output,/Finish any active session/);assert.ok((await fetch(`http://127.0.0.1:${port}/health`)).ok,'existing service was stopped');}finally{await new Promise(resolve=>server.close(resolve));}
 }
 const reserve=createServer();const port=await serverListen(reserve);await new Promise(resolve=>reserve.close(resolve));
 const fresh=await run(port);ownedPid=Number(await readFile(path.join(root,'owned.pid'),'utf8'));assert.equal(fresh.code,0,fresh.output);
 const health=await(await fetch(`http://127.0.0.1:${port}/health`)).json();assert.deepEqual(health,{status:'ok',product:'com.omniharmonic.studiolink',buildId});
 const urlFile=path.join(root,'dashboard-url.txt');
 const reused=await run(port,{STUDIOLINK_PRINT_DASHBOARD_URL_TO:urlFile});assert.equal(reused.code,0,reused.output);assert.equal(Number(await readFile(path.join(root,'owned.pid'),'utf8')),ownedPid,'same-build reuse launched a second daemon');
 // SL-SEC-1: the launcher opens the dashboard signed in with the per-boot launch token, never logging it.
 const launchToken=(await readFile(path.join(root,'data/.launch_token'),'utf8')).trim();
 const dashboardURL=(await readFile(urlFile,'utf8')).trim();assert.equal(dashboardURL,`http://127.0.0.1:${port}/?launch=${launchToken}`);
 assert.ok(!reused.output.includes(launchToken)&&!fresh.output.includes(launchToken),'launcher printed the launch token');
 // A browser navigation (fetch() always sends Sec-Fetch-Mode: cors, which the daemon refuses).
 const signIn=await new Promise((resolve,reject)=>httpGet(dashboardURL,{headers:{'Sec-Fetch-Mode':'navigate'}},res=>{res.resume();resolve(res);}).on('error',reject));
 assert.equal(signIn.statusCode,303);assert.match(String(signIn.headers['set-cookie']||''),/HttpOnly/i);assert.ok(!String(signIn.headers.location).includes('launch'));
 console.log(JSON.stringify({launcherMismatchRefused:3,existingServicesPreserved:true,freshBuildVerified:health,sameBuildReused:true,signedInDashboardURL:true}));
}finally{if(ownedPid)process.kill(ownedPid,'SIGTERM');await rm(root,{recursive:true,force:true});}
