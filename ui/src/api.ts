// The hub's public API, as the UI uses it: the TypeScript client SDK
// (sdk/ts, docs/ui.md §11), and nothing else. The UI has no private
// endpoint, no request of its own (sdkonly.test.ts fails on one), and
// no decision of its own about who may see what: a refusal (401, 403,
// 404, 409) is the API's, and comes back as an ApiError carrying the
// API's own words.
//
// The UI's client is the SDK's default: same-origin, carrying only the
// browser's own credentials (IAP's cookie, or the OIDC session's), so
// no token is read, stored or forwarded, and its event streams are the
// browser's EventSource.
import { HubClient } from '@silo/hub-sdk';

export * from '@silo/hub-sdk';

export const hub = new HubClient();

export const getMe = hub.getMe.bind(hub);
export const setAdminMode = hub.setAdminMode.bind(hub);
export const getAdminMode = hub.getAdminMode.bind(hub);
export const listTasks = hub.listTasks.bind(hub);
export const getTask = hub.getTask.bind(hub);
export const submitTask = hub.submitTask.bind(hub);
export const cancelTask = hub.cancelTask.bind(hub);
export const requeueTask = hub.requeueTask.bind(hub);
export const approveTask = hub.approveTask.bind(hub);
export const rejectTask = hub.rejectTask.bind(hub);
export const listApprovals = hub.listApprovals.bind(hub);
export const answerApproval = hub.answerApproval.bind(hub);
export const listMessages = hub.listMessages.bind(hub);
export const sendMessage = hub.sendMessage.bind(hub);
export const fetchArtifact = hub.fetchArtifact.bind(hub);
export const listNodes = hub.listNodes.bind(hub);
export const listNodeTokens = hub.listNodeTokens.bind(hub);
export const mintNodeToken = hub.mintNodeToken.bind(hub);
export const revokeNodeToken = hub.revokeNodeToken.bind(hub);
export const listTeams = hub.listTeams.bind(hub);
export const getTeam = hub.getTeam.bind(hub);
export const createTeam = hub.createTeam.bind(hub);
export const deleteTeam = hub.deleteTeam.bind(hub);
export const setMember = hub.setMember.bind(hub);
export const removeMember = hub.removeMember.bind(hub);
export const assignRepo = hub.assignRepo.bind(hub);
export const releaseRepo = hub.releaseRepo.bind(hub);
export const getTeamSettings = hub.getTeamSettings.bind(hub);
export const setTeamSettings = hub.setTeamSettings.bind(hub);
export const getGrainfilesOverview = hub.getGrainfilesOverview.bind(hub);
export const listGrainfiles = hub.listGrainfiles.bind(hub);
export const inspectGrainfile = hub.inspectGrainfile.bind(hub);
export const listGrainfileSubscriptions = hub.listGrainfileSubscriptions.bind(hub);
export const listGrainfileEvents = hub.listGrainfileEvents.bind(hub);
