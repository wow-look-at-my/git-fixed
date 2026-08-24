// Writes a git fast-import stream for a synthetic repository.
//
// Usage: node scripts/gen-import.js [commits] [files] [churn] [blobsize] | git fast-import
//
// Building a repository commit by commit spends nearly all its time in process
// startup. fast-import writes the same history in one pass, which is what makes
// a benchmark repository of a few hundred thousand objects practical.
//
// blobsize pads every blob to that many bytes of data that does not compress,
// which is how a repository gets a packfile far larger than its object count
// suggests. That is the shape that decides what a run costs the machine it runs
// on: every byte of a pack is read. see docs/memory.md

const commits = Number(process.argv[2] || 2000);
const files = Number(process.argv[3] || 500);
const churn = Number(process.argv[4] || 40);
const blobsize = Number(process.argv[5] || 0);

// noise is text a packfile can barely shrink. It stays printable ASCII, so a
// character is a byte in what fast-import reads.
//
// Every blob gets its own draw off one running stream, and no two blobs share
// so much as a line. Cutting them all out of one buffer would be quicker and
// would defeat the whole point: two blobs that share a long run are stored as
// one and a delta, which is how gigabytes of blobs become a small packfile.
const noiseBuf = blobsize > 0 ? Buffer.allocUnsafe(blobsize) : null;
let noiseX = 0x9e3779b9;
function noise() {
	for (let i = 0; i < noiseBuf.length; i++) {
		noiseX ^= noiseX << 13;
		noiseX ^= noiseX >>> 17;
		noiseX ^= noiseX << 5;
		noiseBuf[i] = 0x21 + (Math.abs(noiseX) % 94);
	}
	return noiseBuf.toString("latin1");
}

// write puts one buffer down the pipe and waits for it.
//
// process.stdout.write queues what the pipe has not taken yet, and a stream
// that outruns fast-import queues gigabytes of it and dies with ENOBUFS. This
// blocks instead, which is what a generator on the writing end of a pipe wants.
// EAGAIN is the pipe being full, which is the same thing said by the kernel.
const fs = require("node:fs");
const idle = new Int32Array(new SharedArrayBuffer(4));
function write(s) {
	const buf = Buffer.from(s, "utf8");
	for (let at = 0; at < buf.length; ) {
		try {
			at += fs.writeSync(1, buf, at, buf.length - at);
		} catch (err) {
			if (err.code !== "EAGAIN") throw err;
			// Wait for the reader rather than spinning a core against it.
			Atomics.wait(idle, 0, 0, 1);
		}
	}
}

const out = [];
function emit(s) {
	out.push(s);
	if (out.length > 4096) {
		write(out.join(""));
		out.length = 0;
	}
}

// path spreads the files over a directory tree, so the history holds many
// distinct trees rather than one wide one.
function path(i) {
	return `src/${i % 32}/${(i >> 5) % 32}/f${i}.txt`;
}

function blob(i, rev) {
	const head = `file ${i}\nrevision ${rev}\n`;
	if (noiseBuf === null) {
		return `${head}${"x".repeat(64 + ((i * 7 + rev) % 512))}\n`;
	}
	return `${head}${noise()}\n`;
}

let mark = 0;
let parent = 0;
const when = 1700000000;

for (let n = 0; n < commits; n++) {
	const changed = n === 0 ? files : churn;
	const marks = [];
	for (let k = 0; k < changed; k++) {
		const i = n === 0 ? k : (n * churn + k) % files;
		const data = blob(i, n);
		mark++;
		emit(`blob\nmark :${mark}\ndata ${Buffer.byteLength(data)}\n${data}\n`);
		marks.push([mark, i]);
	}
	const msg = `commit ${n}\n`;
	mark++;
	emit(`commit refs/heads/master\nmark :${mark}\n`);
	emit(`committer Bench <bench@example.invalid> ${when + n} +0000\n`);
	emit(`data ${Buffer.byteLength(msg)}\n${msg}`);
	if (parent) emit(`from :${parent}\n`);
	parent = mark;
	for (const [m, i] of marks) emit(`M 100644 :${m} ${path(i)}\n`);
	emit("\n");
}

emit("done\n");
write(out.join(""));
