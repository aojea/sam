// An agent on the mesh: joins, then answers A2A requests from other members
// until stopped. It publishes nothing. There is no service name to look up; a
// caller reaches the agent by its peer ID, through a router, as a2a://agent.
// The mesh policy decides which members may call; the SDK turns the others
// away before anything reaches this code.
//
//   node agent.js                        # answered by the handler below
//   node agent.js http://127.0.0.1:9999  # forwarded to an A2A server beside it
//
// SAM_CONTROL_PLANE_URL names the mesh. The first run enrolls with the file
// SAM_BOOTSTRAP_TOKEN_PATH (a token the mesh operator gave you) or
// SAM_JWT_PATH (a workload identity token your platform issues, such as a
// Kubernetes projected service account token), and keeps the identity and
// credential in SAM_STATE_DIR; later runs resume from there, as the same
// peer, without it.
import { homedir } from "node:os";
import { AgentMesh } from "@sam-mesh/sdk";

const [backendURL] = process.argv.slice(2);

const mesh = await AgentMesh.enroll({
  controlPlaneUrl: process.env.SAM_CONTROL_PLANE_URL ?? "https://mesh.example.com",
  bootstrapTokenPath: process.env.SAM_BOOTSTRAP_TOKEN_PATH,
  jwtPath: process.env.SAM_JWT_PATH,
  stateDir: process.env.SAM_STATE_DIR ?? `${homedir()}/.config/sam-mesh/agent`,
  // A plaintext http:// control plane is otherwise accepted only on loopback.
  allowInsecure: process.env.SAM_INSECURE_CONTROL_PLANE === "true",
});
const session = await mesh.join();

// Answers every path with who was asked and who asked; a real agent runs an
// A2A server here, or beside this process at backendURL.
const target = await session.acceptA2A(
  backendURL !== undefined
    ? { url: backendURL }
    : { handler: (request, caller) => Response.json({ name: "agent", path: new URL(request.url).pathname, caller: caller.peerId }) },
);
console.log(`accepting ${target} as ${session.peerId}`);

const stop = () => void session.close().then(() => process.exit(0));
process.on("SIGINT", stop);
process.on("SIGTERM", stop);
