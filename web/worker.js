// peregrine.skua.lol: the short invite. Discord's bare install link asks for the
// app's Default Install Settings, which peregrine writes at boot from
// core.Permissions, so this never has to change when a feature needs a new bit.
export default {
	fetch(_req, env) {
		return Response.redirect(`https://discord.com/oauth2/authorize?client_id=${env.APP_ID}`, 302);
	},
};
