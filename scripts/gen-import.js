// Writes a git fast-import stream for a synthetic repository.
//
// Usage: node scripts/gen-import.js [commits] [files] [churn] | git fast-import
//
// Building a repository commit by commit spends nearly all its time in process
// startup. fast-import writes the same history in one pass, which is what makes
// a benchmark repository of a few hundred thousand objects practical.

const commits = Number(process.argv[2] || 2000);
const files = Number(process.argv[3] || 500);
const churn = Number(process.argv[4] || 40);

const out = [];
function emit(s) {
	out.push(s);
	if (out.length > 4096) {
		process.stdout.write(out.join(""));
		out.length = 0;
	}
}

// path spreads the files over a directory tree, so the history holds many
// distinct trees rather than one wide one.
function path(i) {
	return `src/${i % 32}/${(i >> 5) % 32}/f${i}.txt`;
}

function blob(i, rev) {
	return `file ${i}\nrevision ${rev}\n${"x".repeat(64 + ((i * 7 + rev) % 512))}\n`;
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
process.stdout.write(out.join(""));
