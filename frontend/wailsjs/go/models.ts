export namespace store {
	
	export class Item {
	    id: string;
	    threadId: string;
	    turnIndex: number;
	    itemIndex: number;
	    kind: string;
	    role: string;
	    summary: string;
	    payloadId?: string;
	    createdAt: number;
	
	    static createFrom(source: any = {}) {
	        return new Item(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.threadId = source["threadId"];
	        this.turnIndex = source["turnIndex"];
	        this.itemIndex = source["itemIndex"];
	        this.kind = source["kind"];
	        this.role = source["role"];
	        this.summary = source["summary"];
	        this.payloadId = source["payloadId"];
	        this.createdAt = source["createdAt"];
	    }
	}
	export class PayloadMeta {
	    id: string;
	    kind: string;
	    meta: string;
	    createdAt: number;
	
	    static createFrom(source: any = {}) {
	        return new PayloadMeta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.meta = source["meta"];
	        this.createdAt = source["createdAt"];
	    }
	}
	export class Thread {
	    id: string;
	    title: string;
	    provider: string;
	    sessionRef?: string;
	    workspacePath: string;
	    model: string;
	    createdAt: number;
	    updatedAt: number;
	    archived: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Thread(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.provider = source["provider"];
	        this.sessionRef = source["sessionRef"];
	        this.workspacePath = source["workspacePath"];
	        this.model = source["model"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	        this.archived = source["archived"];
	    }
	}

}

